FROM python:3.12-slim

WORKDIR /app

# Shared browser location so the non-root runtime user can find them.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers

# Install system dependencies required by Playwright Chromium.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Install Playwright Chromium browser and its OS dependencies to the shared path.
RUN playwright install chromium --with-deps

COPY main.py .

# Create non-root user for security.
RUN useradd -m solver && chown -R solver:solver /opt/pw-browsers /app
USER solver

ENV PORT=8080
EXPOSE 8080

CMD ["python", "main.py"]
