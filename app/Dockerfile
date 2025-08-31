# ---- Build Stage ----
# This stage will install dependencies into a virtual environment.
# We use a specific version of the slim image for reproducibility.
FROM python:3.13-slim-bookworm as builder

# Set an environment variable to prevent writing .pyc files
ENV PYTHONDONTWRITEBYTECODE 1
# Ensure python outputs are sent straight to the terminal without buffering
ENV PYTHONUNBUFFERED 1

# Set the working directory for the virtual environment
WORKDIR /opt/venv

# Create a virtual environment
RUN python -m venv .

# Copy the requirements file
COPY requirements.txt .

# Activate the virtual environment and install dependencies
RUN . /opt/venv/bin/activate && pip install --no-cache-dir -r requirements.txt


# ---- Runtime Stage ----
# This is the final, lean image that will run the application.
FROM python:3.13-slim-bookworm

# Set the same environment variables
ENV PYTHONDONTWRITEBYTECODE 1
ENV PYTHONUNBUFFERED 1

# Create a non-root user for security
RUN useradd --system -U joney-bot

# Set the working directory for the application code
WORKDIR /home/joney-bot/app

# Copy the virtual environment from the 'builder' stage
COPY --from=builder /opt/venv /opt/venv

# Copy only the necessary application source code, not the whole directory.
# This prevents copying unnecessary files like READMEs, .git, etc.
COPY ./base ./base
COPY ./tools ./tools
COPY ./main.py ./

RUN chown -R joney-bot:joney-bot /home/joney-bot /opt/venv

# Set the user to the non-root user
USER joney-bot

# Add the virtual environment's bin directory to the PATH.
# This ensures that the 'python' command uses the venv's interpreter.
ENV PATH="/opt/venv/bin:$PATH"

# Set the command to run the application
CMD ["python", "main.py"]
