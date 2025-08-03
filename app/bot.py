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

        # --- MODIFIED PART: Get the username ---
        username = str(message.author)
        print(f"Received prompt from {username}: '{prompt}'")

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
                    print(
                        "Response is longer than 2000 characters, splitting into chunks."
                    )
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
