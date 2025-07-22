# Joney Discord Bot

## About

I like the idea of a chatbot for discord, simply mention the bot user and it will sanitize and return the AI's response for a fun chatbot

## Todo

### Problems:

- Use a modelfile to create a customized model
- Queue system for multiple request
- Online status for discord
- Consoldate startup task

## Setup

### Create & Activate VENV

- python -m venv venv

- source venv/bin/activate

### Install / Freeze Requirements

- pip install -r requirements.txt

- pip freeze > requirements.txt

### Run the server

- uvicorn api-wrapper:app --reload --host 0.0.0.0 --port 8000

### Run the bot

- python bot.py
