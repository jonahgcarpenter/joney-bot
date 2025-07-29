import os

import discord
import requests
from discord.ext import commands
from dotenv import load_dotenv

# --- Configuration ---
load_dotenv()
TOKEN = os.getenv("DISCORD_TOKEN")
API_WRAPPER_URL = "http://localhost:8000/generate"

# --- Bot Setup ---
intents = discord.Intents.default()
intents.message_content = True

bot = commands.Bot(command_prefix="!", intents=intents)


@bot.event
async def on_ready():
    """Fires when the bot is connected and ready."""
    print(f"Bot is ready! Logged in as {bot.user}")
    print("Listening for @mentions...")


@bot.event
async def on_message(message: discord.Message):
    """Fires on every message sent in a channel the bot can see."""

    if message.author == bot.user:
        return

    if bot.user.mentioned_in(message):
        prompt = message.content.replace(f"<@{bot.user.id}>", "").strip()
        if not prompt:
            return

        print(f"Received prompt from {message.author}: '{prompt}'")

        async with message.channel.typing():
            try:
                response = requests.post(
                    API_WRAPPER_URL, json={"prompt": prompt}, timeout=70
                )
                response.raise_for_status()

                api_data = response.json()
                model_response = api_data.get(
                    "response", "Sorry, I received an empty response."
                )

                # Check if the response is too long and split it if necessary.
                if len(model_response) > 2000:
                    print(
                        "Response is longer than 2000 characters, splitting into chunks."
                    )
                    first_chunk = model_response[:1990]
                    await message.reply(first_chunk)

                    remaining_response = model_response[1990:]
                    for i in range(0, len(remaining_response), 1990):
                        chunk = remaining_response[i : i + 1990]
                        await message.channel.send(chunk)
                else:
                    # If the response is short enough, send it as a single message.
                    await message.reply(model_response)

            except requests.exceptions.HTTPError as http_err:
                if http_err.response.status_code == 429:
                    await message.reply(
                        "You are sending requests too quickly! Please wait a moment."
                    )
                else:
                    error_detail = http_err.response.json().get(
                        "detail", "An unknown error occurred."
                    )
                    await message.reply(
                        f"An error occurred with the API: {error_detail}"
                    )
            except requests.exceptions.RequestException as e:
                await message.reply(
                    "I couldn't connect to my brain (the API wrapper). Please check if it's running."
                )
                print(f"Error connecting to API wrapper: {e}")
            except Exception as e:
                await message.reply(
                    "An unexpected error occurred. Please check the logs."
                )
                print(f"An unexpected error in bot's on_message: {e}")


bot.run(TOKEN)
