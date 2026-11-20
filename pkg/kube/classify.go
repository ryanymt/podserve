package kube

// classify.go holds Diagnose, the pure classifier from one model's serving objects and the time to
// a Diagnosis (D-025): where the pods are, whether they have failed, and why. WaitReady, `podserve
// status` and the v0.2 operator all read it; docs/design/cli.md ("Readiness", "Waiting reasons",
// "A model that can't start") is the contract, and the states in testdata/kube/failures/ are the
// examples.

import (
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// revisionAnnotation is how a Deployment's ReplicaSets are ordered: the highest revision is the
// newest pod template.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// pullEventPrefix marks the kubelet event carrying the registry's own pull error.
const pullEventPrefix = "Failed to pull image"

// stageRank orders the stages by how early in a pod's start-up they are.
var stageRank = map[Stage]int{StageNode: 0, StageImage: 1, StageLoading: 2, StageReady: 3}

// livePod is a live pod of the newest template, with the stage it is waiting at.
type livePod struct {
	pod   *unstructured.Unstructured
	stage Stage
}

// Diagnose says where a model's serving pods are, whether they have failed, and why (D-025). It is
// pure: it reads s and now, and touches nothing. Only the newest pod template's pods count (D-036).
func Diagnose(s Serving, now time.Time) Diagnosis {
	if s.Deployment == nil {
		return Diagnosis{Stage: StageNode}
	}
	// D-036: until the controller has observed this generation, no status says anything about the
	// new template yet.
	observed, _, _ := unstructured.NestedInt64(s.Deployment.Object, "status", "observedGeneration")
	if observed < s.Deployment.GetGeneration() {
		return Diagnosis{Stage: StageNode}
	}
	// D-036: ready when every replica of the newest template is Ready.
	want := int64(1)
	if n, ok, _ := unstructured.NestedInt64(s.Deployment.Object, "spec", "replicas"); ok {
		want = n
	}
	updated, _, _ := unstructured.NestedInt64(s.Deployment.Object, "status", "updatedReplicas")
	ready, _, _ := unstructured.NestedInt64(s.Deployment.Object, "status", "readyReplicas")
	replicas, _, _ := unstructured.NestedInt64(s.Deployment.Object, "status", "replicas")
	if want > 0 && updated == want && ready == want && replicas == want {
		return Diagnosis{Stage: StageReady, Ready: true}
	}

	live, ended := newestPods(s)

	// The diagnosis's stage is the earliest among the live pods, node when there are none.
	stage := StageNode
	best := stageRank[StageLoading] + 1 // nothing live yet, so node wins
	at := make([]livePod, 0, len(live))
	for _, p := range live {
		lp := livePod{pod: p, stage: podStage(p)}
		at = append(at, lp)
		if stageRank[lp.stage] < best {
			stage, best = lp.stage, stageRank[lp.stage]
		}
	}

	// A failure stops the deploy; the first failing pod wins. An image pull still inside the grace
	// period is no failure: it is kept as the ErrImagePull waiting reason for below.
	var retry *Diagnosis
	for _, lp := range at {
		d, done := containerFailure(lp.pod, stage, s.Events, now)
		if !done {
			continue
		}
		if d.Failed {
			return d
		}
		if retry == nil {
			retry = &d
		}
	}

	// Waiting reasons, when nothing failed: the first rule that applies wins (D-026).
	// 1. Autopilot could not or would not add a node for a pod still waiting for one.
	if d, ok := scaleUpReason(at, s.Events, []string{"FailedScaleUp", "NotTriggerScaleUp"}, ""); ok {
		return d
	}
	// 2. A pod of the newest template ended before Ready (a spot reclaim); its ReplicaSet starts
	// another.
	if d, ok := reclaimed(ended, stage); ok {
		return d
	}
	// 3. An image pull that has been failing for less than ImagePullGrace.
	if retry != nil {
		return *retry
	}
	// 4. Autopilot is adding a node for a pod still waiting for one.
	if d, ok := scaleUpReason(at, s.Events, []string{"TriggeredScaleUp"}, "Autopilot is adding a node"); ok {
		return d
	}
	return Diagnosis{Stage: stage}
}

// newestPods splits the newest pod template's pods into the live and the ended ones (D-036): pods
// of other templates and terminating pods don't count, and a pod in phase Failed or Succeeded has
// ended. With no ReplicaSets, every non-deleted pod counts.
func newestPods(s Serving) (live, ended []*unstructured.Unstructured) {
	hash := ""
	if rs := newestReplicaSet(s.ReplicaSets); rs != nil {
		hash = rs.GetLabels()["pod-template-hash"]
	}
	for i := range s.Pods {
		p := &s.Pods[i]
		if p.GetDeletionTimestamp() != nil {
			continue
		}
		if len(s.ReplicaSets) > 0 && p.GetLabels()["pod-template-hash"] != hash {
			continue
		}
		phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
		if phase == "Failed" || phase == "Succeeded" {
			ended = append(ended, p)
		} else {
			live = append(live, p)
		}
	}
	return live, ended
}

// newestReplicaSet returns the ReplicaSet with the highest revision annotation, or nil when there
// are none.
func newestReplicaSet(rss []unstructured.Unstructured) *unstructured.Unstructured {
	var newest *unstructured.Unstructured
	for i := range rss {
		rs := &rss[i]
		if newest != nil && revision(rs) <= revision(newest) {
			continue
		}
		newest = rs
	}
	return newest
}

// revision reads a ReplicaSet's revision annotation (an integer string); an unparsable one counts
// as no revision.
func revision(rs *unstructured.Unstructured) int {
	n, err := strconv.Atoi(rs.GetAnnotations()[revisionAnnotation])
	if err != nil {
		return 0
	}
	return n
}

// podStage says how far one live pod has got, the earliest stage among its containers: node while
// it has nowhere to run, image while a container is waiting to start for the first time, loading
// once one runs.
func podStage(p *unstructured.Unstructured) Stage {
	if conditionStatus(p.Object, "PodScheduled") != "True" {
		return StageNode
	}
	statuses := containerStatuses(p)
	if len(statuses) == 0 {
		return StageImage
	}
	stage := StageLoading
	for _, cs := range statuses {
		csStage := StageLoading
		if _, waiting, _ := unstructured.NestedMap(cs, "state", "waiting"); waiting {
			restarts, _, _ := unstructured.NestedInt64(cs, "restartCount")
			_, ran, _ := unstructured.NestedMap(cs, "lastState", "terminated")
			if restarts == 0 && !ran {
				csStage = StageImage // a container that has never run is still getting its image
			}
		}
		if stageRank[csStage] < stageRank[stage] {
			stage = csStage
		}
	}
	return stage
}

// containerStatuses lists a pod's container statuses.
func containerStatuses(p *unstructured.Unstructured) []map[string]any {
	statuses, found, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
	if !found {
		return nil
	}
	var out []map[string]any
	for _, s := range statuses {
		if cs, ok := s.(map[string]any); ok {
			out = append(out, cs)
		}
	}
	return out
}

// containerFailure reads one live pod's containers. A container waiting on a reason that fails the
// deploy gives its Diagnosis (done with Failed); an image pull still inside ImagePullGrace gives
// the ErrImagePull waiting reason (done without Failed); anything else is not done.
func containerFailure(p *unstructured.Unstructured, stage Stage, events []unstructured.Unstructured,
	now time.Time,
) (d Diagnosis, done bool) {
	for _, cs := range containerStatuses(p) {
		waiting, found, _ := unstructured.NestedMap(cs, "state", "waiting")
		if !found {
			continue
		}
		msg := collapse(firstString(waiting, "message"))
		switch firstString(waiting, "reason") {
		case "CrashLoopBackOff":
			d = Diagnosis{Stage: stage, Failed: true, Reason: ReasonCrashLoop, Message: msg, Pod: p.GetName()}
			if term, ok, _ := unstructured.NestedMap(cs, "lastState", "terminated"); ok {
				t := Termination{Reason: firstString(term, "reason")}
				if code, ok := intValue(term["exitCode"]); ok {
					t.ExitCode = int32(code)
				}
				d.Last = &t
			}
			return d, true
		case "InvalidImageName":
			return Diagnosis{Stage: stage, Failed: true, Reason: ReasonInvalidImage, Message: msg, Pod: p.GetName()}, true
		case "CreateContainerConfigError":
			return Diagnosis{Stage: stage, Failed: true, Reason: ReasonCreateConfig, Message: msg, Pod: p.GetName()}, true
		case "CreateContainerError":
			return Diagnosis{Stage: stage, Failed: true, Reason: ReasonCreateContainer, Message: msg, Pod: p.GetName()}, true
		case "ErrImagePull", "ImagePullBackOff":
			return imagePull(p, msg, stage, eventsAbout(events, p.GetName()), now), true
		}
	}
	return Diagnosis{}, false
}

// imagePull classifies a pod whose image pull is failing. The message is the registry's own error:
// the latest Failed event that starts with "Failed to pull image", else the container's waiting
// message. The pull fails the deploy once it has been failing for ImagePullGrace, counted from the
// pod's first Failed event; before that it is the ErrImagePull waiting reason and deploy keeps
// waiting (D-026).
func imagePull(p *unstructured.Unstructured, waitingMsg string, stage Stage,
	events []unstructured.Unstructured, now time.Time,
) Diagnosis {
	msg := waitingMsg
	if ev := latestEvent(events, func(o map[string]any) bool {
		return o["reason"] == "Failed" && strings.HasPrefix(firstString(o, "message"), pullEventPrefix)
	}); ev != nil {
		msg = collapse(firstString(ev.Object, "message"))
	}
	if first, ok := earliestFailed(events); ok && now.Sub(first) >= ImagePullGrace {
		return Diagnosis{Stage: stage, Failed: true, Reason: ReasonImagePull, Message: msg, Pod: p.GetName()}
	}
	return Diagnosis{Stage: stage, Reason: ReasonPullRetry, Message: msg, Pod: p.GetName()}
}

// scaleUpReason reports a live pod that is still waiting for its node with an Autopilot scale-up
// event about it, the latest by lastTimestamp; a scheduled pod's scale-up events are stale, and
// FailedScheduling events never count. fixedMsg replaces the event's message when set
// (TriggeredScaleUp: Autopilot's own text is a list of instance groups).
func scaleUpReason(live []livePod, events []unstructured.Unstructured, reasons []string, fixedMsg string) (Diagnosis, bool) {
	for _, lp := range live {
		if lp.stage != StageNode {
			continue
		}
		ev := latestEvent(eventsAbout(events, lp.pod.GetName()), func(o map[string]any) bool {
			for _, r := range reasons {
				if o["reason"] == r {
					return true
				}
			}
			return false
		})
		if ev == nil {
			continue
		}
		msg := collapse(firstString(ev.Object, "message"))
		if fixedMsg != "" {
			msg = fixedMsg
		}
		return Diagnosis{Stage: lp.stage, Reason: Reason(firstString(ev.Object, "reason")),
			Message: msg, Pod: lp.pod.GetName()}, true
	}
	return Diagnosis{}, false
}

// reclaimed reports an ended pod the kubelet disrupted before it was Ready (its DisruptionTarget
// condition): a spot reclaim, whose GPU node is gone; the ReplicaSet starts another.
func reclaimed(ended []*unstructured.Unstructured, stage Stage) (Diagnosis, bool) {
	for _, p := range ended {
		conds, found, _ := unstructured.NestedSlice(p.Object, "status", "conditions")
		if !found {
			continue
		}
		for _, c := range conds {
			cond, ok := c.(map[string]any)
			if !ok || cond["type"] != "DisruptionTarget" || cond["status"] != "True" {
				continue
			}
			return Diagnosis{Stage: stage, Reason: ReasonReclaimed,
				Message: collapse(firstString(cond, "message")), Pod: p.GetName()}, true
		}
	}
	return Diagnosis{}, false
}

// eventsAbout keeps an object's events. The API server would filter by field selector; Serving
// reads the namespace's whole list (the fake ignores selectors), so Diagnose filters by
// involvedObject itself.
func eventsAbout(events []unstructured.Unstructured, name string) []unstructured.Unstructured {
	var out []unstructured.Unstructured
	for i := range events {
		if about, _, _ := unstructured.NestedString(events[i].Object, "involvedObject", "name"); about == name {
			out = append(out, events[i])
		}
	}
	return out
}

// latestEvent returns the event with the greatest lastTimestamp among those that match, nil when
// none does. An event without a parsable lastTimestamp counts as the oldest.
func latestEvent(events []unstructured.Unstructured, match func(map[string]any) bool) *unstructured.Unstructured {
	var latest *unstructured.Unstructured
	best := time.Time{}
	for i := range events {
		if !match(events[i].Object) {
			continue
		}
		at, _ := timeAt(&events[i], "lastTimestamp")
		if latest == nil || at.After(best) {
			latest, best = &events[i], at
		}
	}
	return latest
}

// earliestFailed returns the earliest firstTimestamp among the events with reason Failed: when an
// image pull has been failing since.
func earliestFailed(events []unstructured.Unstructured) (time.Time, bool) {
	var first time.Time
	found := false
	for i := range events {
		if events[i].Object["reason"] != "Failed" {
			continue
		}
		at, ok := timeAt(&events[i], "firstTimestamp")
		if !ok {
			continue
		}
		if !found || at.Before(first) {
			first, found = at, true
		}
	}
	return first, found
}

// timeAt parses an event's RFC3339 timestamp field (a metav1.Time as stored in an unstructured
// object); ok is false when it is absent or null.
func timeAt(u *unstructured.Unstructured, field string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, firstString(u.Object, field))
	return t, err == nil
}

// collapse trims a Kubernetes message to one line without cutting it: the CLI cuts its one-line
// reason output, the diagnosis keeps the whole message (an image error's cause comes at its end).
func collapse(msg string) string {
	return strings.Join(strings.Fields(msg), " ")
}

// intValue reads an integer the API machinery decoded (int64 in unstructured objects, float64 in
// hand-built ones).
func intValue(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}
