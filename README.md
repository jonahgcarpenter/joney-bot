# Joney Discord Bot

## About

I like the idea of a chatbot for discord, simply mention the bot user and it will sanitize and return the AI's response for a fun chatbot

## Todo

### Problems:

- Train my own LLM, provided ones are less maluable and are by nature less unhinged
- Queue system for multiple request
- Memory and User context
- Ensure the search function won't get me in trouble, maybe save each search query too?
- Fix prod deployment showing up as Stack in portainer
- Shrink container size??

## Python Usage

- cd app

### Create & Activate VENV

- python -m venv venv

- source venv/bin/activate

### Install Requirements

- pip install -r requirements.txt

### Run the server

- python main.py
