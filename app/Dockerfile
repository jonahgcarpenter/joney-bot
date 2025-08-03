FROM python:3.13-slim

RUN useradd --system -U joney-bot

WORKDIR /home/joney-bot/app

COPY requirements.txt .

RUN pip install --no-cache-dir -r requirements.txt

COPY . .

RUN chown -R joney-bot:joney-bot /home/joney-bot

USER joney-bot

CMD ["python", "main.py"]
