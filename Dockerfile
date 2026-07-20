FROM python:3.12-slim

WORKDIR /app

# Install system dependencies required by Playwright Chromium.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Install Playwright Chromium browser and its OS dependencies.
RUN playwright install chromium --with-deps

COPY main.py .

# Create non-root user for security.
RUN useradd -m solver
USER solver

ENV PORT=8080
EXPOSE 8080

CMD ["python", "main.py"]
