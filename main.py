import multiprocessing
import subprocess
import sys
import time


def run_fastapi():
    """Starts the FastAPI application using uvicorn."""
    print("--- Starting API Wrapper (FastAPI) ---")
    # Use sys.executable to ensure the same Python interpreter is used
    process = subprocess.Popen(
        [sys.executable, "-m", "uvicorn", "api-wrapper:app"],
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    process.wait()


def run_discord_bot():
    """Starts the Discord bot."""
    print("--- Starting Discord Bot ---")
    # Use sys.executable to ensure the same Python interpreter is used
    process = subprocess.Popen(
        [sys.executable, "bot.py"],
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    process.wait()


if __name__ == "__main__":
    # Create processes for both the API and the bot
    api_process = multiprocessing.Process(target=run_fastapi)
    bot_process = multiprocessing.Process(target=run_discord_bot)

    print("--- Launching services ---")
    # Start both processes
    api_process.start()
    bot_process.start()

    try:
        # Keep the main script alive while the subprocesses are running
        while True:
            time.sleep(1)
    except KeyboardInterrupt:
        print("\n--- Shutting down services ---")
        # Terminate the processes on exit
        api_process.terminate()
        bot_process.terminate()
        api_process.join()
        bot_process.join()
        print("--- Services stopped ---")
