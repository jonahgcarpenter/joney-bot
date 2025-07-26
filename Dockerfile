FROM python:3.13-slim

RUN useradd --system -U oswald

WORKDIR /app

COPY requirements.txt .

RUN pip install --no-cache-dir -r requirements.txt

COPY . .

RUN chown -R oswald:oswald /app

USER oswald

CMD ["python", "main.py"]
