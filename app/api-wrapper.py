import os
import re

import requests
import vector_db
from dotenv import load_dotenv
from fastapi import BackgroundTasks, FastAPI, HTTPException, Request
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

load_dotenv()

# --- Configuration ---
OLLAMA_HOST = os.getenv("OLLAMA_HOST_URL")

# --- Initialize Model for Embeddings ---
print("Loading sentence transformer model...")
embedding_model = SentenceTransformer(
    "nomic-ai/nomic-embed-text-v1.5", trust_remote_code=True
)
print("Model loaded.")

app = FastAPI()


# --- Pydantic Model for Input Validation ---
class PromptRequest(BaseModel):
    prompt: str
    username: str
    model: str = "joney-bot:latest"


# --- Input Sanitization Function ---
def sanitize_input(prompt: str) -> str:
    """A simple sanitizer to remove potentially harmful characters."""
    sanitized = re.sub(r"[^a-zA-Z0-9\s.,!?-]", "", prompt)
    return sanitized.strip()


# --- Background Task for Saving to DB ---
def save_to_db_background(username: str, prompt: str, response: str):
    """Generates embeddings and saves the chat log to the database."""
    print(f"Generating embeddings for prompt and response from user '{username}'...")
    prompt_embedding = embedding_model.encode(prompt)
    response_embedding = embedding_model.encode(response)
    print("Embeddings generated. Saving to database...")
    vector_db.save_chat(
        username, prompt, response, prompt_embedding, response_embedding
    )


# --- API Endpoint ---
@app.post("/generate")
async def generate_prompt(
    request: Request, data: PromptRequest, background_tasks: BackgroundTasks
):
    """Receives a prompt, gets a response from Ollama, and saves the exchange to the DB."""
    sanitized_prompt = sanitize_input(data.prompt)

    if not sanitized_prompt:
        raise HTTPException(
            status_code=400, detail="Prompt is empty after sanitization."
        )

    print(
        f"Received sanitized prompt from '{data.username}': '{sanitized_prompt}' for model '{data.model}'"
    )

    try:
        response = requests.post(
            f"{OLLAMA_HOST}/api/generate",
            json={"model": data.model, "prompt": sanitized_prompt, "stream": False},
            timeout=60,
        )
        response.raise_for_status()

        ollama_data = response.json()
        model_response = ollama_data.get("response", "No response content from model.")

        background_tasks.add_task(
            save_to_db_background, data.username, sanitized_prompt, model_response
        )

        return {"response": model_response}

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


@app.on_event("startup")
async def startup_event():
    """On startup, set up the database."""
    print("--- Running database setup ---")
    vector_db.setup_database()


@app.get("/")
def read_root():
    return {"status": "Ollama API Wrapper is running"}
