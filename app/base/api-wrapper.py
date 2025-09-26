import logging
import os
import re
import sys

# --- Path Setup ---
project_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, project_root)

# Import and run logging config before anything else
from tools.logging_config import setup_logging

setup_logging()

from dotenv import load_dotenv

load_dotenv()

import requests
from fastapi import BackgroundTasks, FastAPI, HTTPException, Request
from pydantic import BaseModel
from tools import intent_analysis, search, vector_db
from tools.system_prompts import (
    get_final_answer_prompt,
    get_user_profile_generator_prompt,
    get_user_profile_updater_prompt,
)

# --- Logging Setup ---
log = logging.getLogger(__name__)


# --- Configuration ---
OLLAMA_HOST = os.getenv("OLLAMA_HOST_URL")
OLLAMA_EMBEDDING_MODEL = os.getenv("OLLAMA_EMBEDDING_MODEL", "nomic-embed-text:v1.5")
CONTEXT_SUMMARY_COUNT = int(os.getenv("CONTEXT_SUMMARY_COUNT", 10))

# --- Initialize App ---
app = FastAPI()


# --- Pydantic Model for Input Validation ---
class PromptRequest(BaseModel):
    prompt: str
    username: str
    model: str = "llama2-uncensored:7b"
    target_user: str | None = None


# --- Input Sanitization Function ---
def sanitize_input(prompt: str) -> str:
    """A simple sanitizer to remove potentially harmful characters."""
    sanitized = re.sub(r"[^a-zA-Z0-9\s.,!?-]", "", prompt)
    return sanitized.strip()


def get_ollama_embedding(text_to_embed: str, model: str) -> list[float]:
    """Generates an embedding for a given text using the Ollama API."""
    try:
        response = requests.post(
            f"{OLLAMA_HOST}/api/embeddings",
            json={"model": model, "prompt": text_to_embed},
            timeout=30,
        )
        response.raise_for_status()
        return response.json().get("embedding")
    except requests.RequestException as e:
        log.error(f"Failed to get embedding from Ollama for model '{model}': {e}")
        raise


# --- Background Task for Saving and Profiling ---
def process_and_save_background(
    username: str,
    prompt: str,
    response: str,
    model: str,
    search_queries: list[str] | None = None,
):
    """
    Saves chat, then generates or updates the user profile based on context.
    """
    try:
        # Save the current interaction
        log.debug(f"Saving current chat for '{username}'.")
        log.debug(
            f"Generating embeddings with Ollama model '{OLLAMA_EMBEDDING_MODEL}'."
        )
        # Call the new function to get embeddings from Ollama
        prompt_embedding = get_ollama_embedding(prompt, OLLAMA_EMBEDDING_MODEL)
        response_embedding = get_ollama_embedding(response, OLLAMA_EMBEDDING_MODEL)

        vector_db.save_chat(
            username,
            prompt,
            response,
            prompt_embedding,
            response_embedding,
            search_queries,
        )

        # Check for existing user context/profile
        log.debug(f"Checking for existing profile for '{username}'.")
        existing_profile = vector_db.get_user_context(username)
        profile_prompt = None

        if not existing_profile:
            # --- CASE 1: No existing profile. Create one from the last 10 chats. ---
            log.info(f"No profile found for '{username}'. Generating a new one.")
            chat_history = vector_db.get_recent_chats(username, CONTEXT_SUMMARY_COUNT)
            if chat_history:
                profile_prompt = get_user_profile_generator_prompt(
                    chat_history, username
                )
            else:
                log.warning(
                    f"No chat history for '{username}', skipping profile generation."
                )
                return
        else:
            # --- CASE 2: Profile exists. Update it with the single most recent chat. ---
            log.info(f"Existing profile found for '{username}'. Updating it.")
            most_recent_chat = vector_db.get_single_most_recent_chat(username)
            if most_recent_chat:
                profile_prompt = get_user_profile_updater_prompt(
                    existing_profile, most_recent_chat, username
                )
            else:
                log.warning(
                    f"Could not fetch most recent chat for '{username}', skipping profile update."
                )
                return

        # Generate the new/updated profile
        log.info(f"Generating new/updated user profile for '{username}'.")
        profile_response = requests.post(
            f"{OLLAMA_HOST}/api/generate",
            json={"model": model, "prompt": profile_prompt, "stream": False},
            timeout=60,
        )
        profile_response.raise_for_status()
        new_profile = profile_response.json().get("response", "").strip()

        # Save the new profile
        if new_profile:
            vector_db.update_user_profile(username, new_profile)
        else:
            log.warning(f"LLM returned an empty profile for '{username}'.")

    except Exception as e:
        log.error(f"Error in background task for '{username}': {e}", exc_info=True)
    finally:
        log.info(f"[bold red]ENDING INTERACTION with {username}[/bold red]")


# --- API Endpoint ---
@app.post("/generate")
async def generate_prompt(
    request: Request, data: PromptRequest, background_tasks: BackgroundTasks
):
    """
    Receives a prompt, gets a response, and kicks off a background task.
    """
    log.info(f"[bold red]STARTING INTERACTION with {data.username}[/bold red]")

    sanitized_prompt = sanitize_input(data.prompt)
    if not sanitized_prompt:
        raise HTTPException(
            status_code=400, detail="Prompt is empty after sanitization."
        )

    try:
        # --- GET USER CONTEXTS ---
        user_context = vector_db.get_user_context(data.username)
        target_user_profile = None
        if data.target_user:
            log.info(f"Prompt is about '{data.target_user}'. Fetching their profile.")
            target_user_profile = vector_db.get_user_context(data.target_user)
            if not target_user_profile:
                log.warning(f"No profile found for target user '{data.target_user}'.")

        # --- INTENT ANALYSIS ---
        search_needed = intent_analysis.decide_if_search_is_needed(
            prompt=sanitized_prompt, model=data.model
        )
        search_context, search_queries = None, None
        if search_needed:
            log.info("Search is needed. Starting intelligent search process.")
            search_context, search_queries = search.think_and_search(
                prompt=sanitized_prompt, model=data.model
            )
        else:
            log.info("Search not needed. Generating a conversational response.")

        # --- GENERATE FINAL RESPONSE ---
        final_prompt = get_final_answer_prompt(
            sanitized_prompt,
            search_context,
            user_context,
            target_user_profile,
            data.target_user,
        )
        response = requests.post(
            f"{OLLAMA_HOST}/api/generate",
            json={"model": data.model, "prompt": final_prompt, "stream": False},
            timeout=60,
        )
        response.raise_for_status()
        model_response = response.json().get("response", "No response from model.")

        # --- KICK OFF BACKGROUND TASK ---
        background_tasks.add_task(
            process_and_save_background,
            data.username,
            sanitized_prompt,
            model_response,
            data.model,
            search_queries,
        )
        return {"response": model_response}

    except Exception as e:
        log.error(
            f"An unexpected error occurred in generate_prompt for '{data.username}': {e}",
            exc_info=True,
        )
        log.info(
            f"[bold red]ENDING INTERACTION with {data.username} due to error[/bold red]"
        )
        raise HTTPException(
            status_code=500, detail="An internal server error occurred."
        )


@app.get("/context/{username}")
async def get_user_context_endpoint(username: str):
    """Fetches the user profile/context from the database."""
    log.info(f"Received request for context for user '{username}'.")
    user_context = vector_db.get_user_context(username)
    if not user_context:
        raise HTTPException(status_code=44, detail="No context found for this user.")
    return {"username": username, "context": user_context}


@app.on_event("startup")
async def startup_event():
    vector_db.setup_database()


@app.get("/health")
def health_check():
    return {"status": "ok"}
