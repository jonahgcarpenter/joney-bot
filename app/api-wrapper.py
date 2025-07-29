import os
import re

import requests
from dotenv import load_dotenv
from fastapi import FastAPI, HTTPException, Request
from pydantic import BaseModel

# from slowapi import Limiter, _rate_limit_exceeded_handler
# from slowapi.errors import RateLimitExceeded
# from slowapi.util import get_remote_address

load_dotenv()

# --- Configuration ---
OLLAMA_HOST = os.getenv("OLLAMA_HOST_URL")

# Initialize Rate Limiter
# limiter = Limiter(key_func=get_remote_address)
app = FastAPI()
# app.state.limiter = limiter
# app.add_exception_handler(RateLimitExceeded, _rate_limit_exceeded_handler)


# --- Pydantic Model for Input Validation ---
class PromptRequest(BaseModel):
    prompt: str
    model: str = "joney-bot:latest"


# --- Input Sanitization Function ---
def sanitize_input(prompt: str) -> str:
    """
    A simple sanitizer to remove potentially harmful characters.
    This is a basic example; adjust based on your security needs.
    """
    # Remove anything that isn't a letter, number, space, or basic punctuation.
    sanitized = re.sub(r"[^a-zA-Z0-9\s.,!?-]", "", prompt)
    return sanitized.strip()


# --- API Endpoint ---
@app.post("/generate")
# @limiter.limit("5/minute")
async def generate_prompt(request: Request, data: PromptRequest):
    """
    Receives a prompt, sanitizes it, and forwards it to Ollama.
    """
    sanitized_prompt = sanitize_input(data.prompt)

    if not sanitized_prompt:
        raise HTTPException(
            status_code=400, detail="Prompt is empty after sanitization."
        )

    print(f"Received sanitized prompt: '{sanitized_prompt}' for model '{data.model}'")

    try:
        # Forward the request to the Ollama API
        response = requests.post(
            f"{OLLAMA_HOST}/api/generate",
            json={
                "model": data.model,
                "prompt": sanitized_prompt,
                "stream": False,
            },
            timeout=60,
        )
        response.raise_for_status()

        ollama_data = response.json()
        return {
            "response": ollama_data.get("response", "No response content from model.")
        }

    except requests.exceptions.RequestException as e:
        print(f"Error contacting Ollama: {e}")
        raise HTTPException(
            status_code=503, detail="Could not connect to the Ollama service."
        )
    except Exception as e:
        print(f"An unexpected error occurred: {e}")
        raise HTTPException(
            status_code=500, detail="An internal server error occurred."
        )


@app.get("/")
def read_root():
    return {"status": "Ollama API Wrapper is running"}
