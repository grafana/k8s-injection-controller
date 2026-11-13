FROM python:3.15.1-alpine@sha256:54448f67d7c31c39e5cdd3ea2d6382395ce142806c284425f8e897d78a0465b3
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app.py .
CMD ["python", "app.py"]
