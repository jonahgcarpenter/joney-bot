import multiprocessing
import subprocess
import sys
import time


def run_fastapi():
    """Starts the FastAPI application using uvicorn."""
    process = subprocess.Popen(
        [sys.executable, "-m", "uvicorn", "api-wrapper:app", "--log-level", "warning"],
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    process.wait()


def run_discord_bot():
    """Starts the Discord bot."""
    process = subprocess.Popen(
        [sys.executable, "bot.py"],
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    process.wait()


if __name__ == "__main__":
    api_process = multiprocessing.Process(target=run_fastapi)
    bot_process = multiprocessing.Process(target=run_discord_bot)

    api_process.start()
    bot_process.start()

    try:
        while True:
            time.sleep(1)
    except KeyboardInterrupt:
        print("\n--- Shutting down services ---")
        api_process.terminate()
        bot_process.terminate()
        api_process.join()
        bot_process.join()
        print("--- Services stopped ---")
