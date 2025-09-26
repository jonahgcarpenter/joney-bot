import asyncio
import logging
import os
import sys
from urllib.parse import quote

project_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, project_root)

from tools.logging_config import setup_logging

setup_logging()

import aiohttp
import discord
from discord.ext import commands
from dotenv import load_dotenv

# --- Configuration ---
load_dotenv()
TOKEN = os.getenv("DISCORD_TOKEN")
API_BASE_URL = "http://localhost:8000"
API_WRAPPER_URL = f"{API_BASE_URL}/generate"
API_HEALTH_URL = f"{API_BASE_URL}/health"
API_CONTEXT_URL = f"{API_BASE_URL}/context"


# --- Logging Setup ---
log = logging.getLogger(__name__)


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
    async with aiohttp.ClientSession() as session:
        for attempt in range(max_retries):
            try:
                # Check the health of the API wrapper using aiohttp
                async with session.get(API_HEALTH_URL, timeout=3) as response:
                    if response.status == 200:
                        data = await response.json()
                        if data.get("status") == "ok":
                            logging.info("Backend API is online and healthy")
                            logging.info(f"Bot is ready! Logged in as {bot.user}")
                            return
            except (aiohttp.ClientConnectorError, asyncio.TimeoutError):
                # This is expected if the API isn't running yet, so we just wait
                pass
            except aiohttp.ClientError as e:
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

    # Ignore any message containing @everyone or @here
    if (
        message.mention_everyone
        or "@everyone" in message.content
        or "@here" in message.content
    ):
        return

    # Check if the bot was specifically mentioned
    if bot.user in message.mentions:
        # --- Clean the prompt by removing the bot's mention ---
        mention_standard = f"<@{bot.user.id}>"
        mention_nickname = f"<@!{bot.user.id}>"
        prompt = (
            message.content.replace(mention_standard, "")
            .replace(mention_nickname, "")
            .strip()
        )

        username = str(message.author)

        # --- Handle empty prompts after cleaning ---
        if not prompt:
            await message.reply("What the fuck do you want idiot?")
            return

        # --- Handle !context command ---
        if prompt == "!context":
            log.info(f"User '{username}' requested their context.")
            async with message.channel.typing():
                try:
                    # URL-encode the username to handle special characters like '#'
                    encoded_username = quote(username)
                    async with aiohttp.ClientSession() as session:
                        async with session.get(
                            f"{API_CONTEXT_URL}/{encoded_username}"
                        ) as response:
                            if response.status == 200:
                                data = await response.json()
                                context = data.get(
                                    "context", "Context data is missing."
                                )
                                reply_message = (
                                    f"Here is your saved context:\n```\n{context}\n```"
                                )
                                await message.reply(reply_message)
                            elif response.status == 404:
                                await message.reply(
                                    "I don't have any context saved for you yet."
                                )
                            else:
                                response.raise_for_status()
                except Exception as e:
                    log.error(f"Error fetching context for '{username}': {e}")
                    await message.reply(
                        "Sorry, I couldn't retrieve your context due to an error."
                    )
            return

        # --- Identify if another single user was mentioned ---
        target_user_name = None
        other_mentions = [m for m in message.mentions if m.id != bot.user.id]
        if len(other_mentions) == 1:
            target_user_name = str(other_mentions[0])

        # --- Resolve all remaining mentions to display names for the API ---
        if message.mentions:
            for member in message.mentions:
                if member.id != bot.user.id:
                    prompt = prompt.replace(member.mention, member.display_name)

        async with message.channel.typing():
            try:
                payload = {"prompt": prompt, "username": username}
                if target_user_name:
                    payload["target_user"] = target_user_name

                async with aiohttp.ClientSession() as session:
                    async with session.post(
                        API_WRAPPER_URL, json=payload, timeout=70
                    ) as response:
                        response.raise_for_status()
                        api_data = await response.json()
                        model_response = api_data.get(
                            "response", "Sorry, I received an empty response."
                        )

                # Split and send the response if it exceeds Discord's character limit
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

            except aiohttp.ClientResponseError as http_err:
                error_detail = "An unknown error occurred."
                try:
                    error_json = await http_err.json()
                    error_detail = error_json.get("detail", error_detail)
                except Exception:
                    pass
                await message.reply(f"An error occurred with the API: {error_detail}")
                logging.error(f"HTTPError: {error_detail} (Status: {http_err.status})")
            except asyncio.TimeoutError:
                await message.reply(
                    "My brain took too long to respond (timeout). Please try again."
                )
                logging.error("API Connection Error: Timeout")
            except aiohttp.ClientConnectorError as e:
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
