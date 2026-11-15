# The fetch job image (P01 T7; docs/design/platform.md "fetch Job interface"). python:3.13-slim
# pinned by digest. Runs as root in v0.1: the scratch volume is root-owned; non-root needs an
# fsGroup, which is later hardening work.
FROM python@sha256:e48f59cdba69a3cbd9ed2cf0f348d90d45a8d350d98134cf1070bc106ee9ed90
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY fetch.py .
ENTRYPOINT ["python", "-I", "/app/fetch.py"]
