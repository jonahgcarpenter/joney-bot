import asyncio
import logging
import os

import discord
import requests
from discord.ext import commands
from dotenv import load_dotenv

# --- Configuration ---
load_dotenv()
TOKEN = os.getenv("DISCORD_TOKEN")
API_WRAPPER_URL = "http://localhost:8000/generate"
API_HEALTH_URL = API_WRAPPER_URL.replace("/generate", "/health")


# --- Logging Setup ---
logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)

# Silence noisy discord.py logs
logging.getLogger("discord").setLevel(logging.WARNING)
logging.getLogger("discord.client").setLevel(logging.WARNING)
logging.getLogger("discord.gateway").setLevel(logging.WARNING)


# --- Bot Setup ---
intents = discord.Intents.default()
intents.message_content = True
bot = commands.Bot(command_prefix="!", intents=intents)


@bot.event
async def on_ready():
    """Fires when connected to Discord, then checks for backend readiness."""
    logging.info(f"Connected to Discord as {bot.user}. Waiting for backend API...")

    max_retries = 12  # Try for up to 60 seconds (12 * 5s)
    for attempt in range(max_retries):
        try:
            # Check the health of the API wrapper
            response = requests.get(API_HEALTH_URL, timeout=3)
            if response.status_code == 200 and response.json().get("status") == "ok":
                logging.info("Backend API is online and healthy")
                # This log now appears last, confirming a fully successful startup
                logging.info(f"Bot is ready! Logged in as {bot.user}")
                return
        except requests.exceptions.ConnectionError:
            # This is expected if the API isn't running yet, so we just wait
            pass
        except requests.exceptions.RequestException as e:
            # Log other errors (like timeouts) but continue to retry
            logging.warning(f"API health check failed: {e}")

        # Wait before retrying
        await asyncio.sleep(5)

    logging.critical(
        "FATAL: Backend API did not become healthy. Bot may not function correctly."
    )


@bot.event
async def on_message(message: discord.Message):
    """Fires on every message sent in a channel the bot can see."""
    if message.author == bot.user:
        return

    if bot.user.mentioned_in(message):
        prompt = message.content.replace(f"<@{bot.user.id}>", "").strip()

        # --- Resolve user mentions ---
        if message.mentions:
            for member in message.mentions:
                if member.id != bot.user.id:
                    prompt = prompt.replace(member.mention, member.display_name)

        username = str(message.author)

        async with message.channel.typing():
            try:
                payload = {"prompt": prompt, "username": username}
                response = requests.post(API_WRAPPER_URL, json=payload, timeout=70)
                response.raise_for_status()

                api_data = response.json()
                model_response = api_data.get(
                    "response", "Sorry, I received an empty response."
                )

                if len(model_response) > 2000:
                    logging.warning("Response > 2000 chars, splitting.")
                    for i in range(0, len(model_response), 1990):
                        chunk = model_response[i : i + 1990]
                        if i == 0:
                            await message.reply(chunk)
                        else:
                            await message.channel.send(chunk)
                else:
                    await message.reply(model_response)

            except requests.exceptions.HTTPError as http_err:
                error_detail = "An unknown error occurred."
                try:
                    error_detail = http_err.response.json().get("detail", error_detail)
                except:
                    pass
                await message.reply(f"An error occurred with the API: {error_detail}")
                logging.error(f"HTTPError: {error_detail}")
            except requests.exceptions.RequestException as e:
                await message.reply(
                    "I couldn't connect to my brain (the API wrapper). Please check if it's running."
                )
                logging.error(f"API Connection Error: {e}")
            except Exception as e:
                await message.reply(
                    "An unexpected error occurred. Please check the logs."
                )
                logging.error(f"Unexpected error in on_message: {e}", exc_info=True)


bot.run(TOKEN)
