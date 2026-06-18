FROM python:3.11.15-alpine@sha256:cf0253107a1e77e63c814a05428308965a47eb44a2d62fc828564c4a8c839fab
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app.py .
CMD ["python", "app.py"]
