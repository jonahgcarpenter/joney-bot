import multiprocessing
import os
import signal
import subprocess
import sys
import time


def run_fastapi():
    """Starts the FastAPI application using uvicorn."""
    process = subprocess.Popen(
        [
            sys.executable,
            "-m",
            "uvicorn",
            "base.api-wrapper:app",
            "--log-level",
            "warning",
            "--reload",
        ],
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    # Wait for the subprocess to complete, but handle the interrupt gracefully.
    try:
        process.wait()
    except KeyboardInterrupt:
        print("FastAPI process interrupted, terminating uvicorn.")
        process.terminate()
        process.wait()  # Wait for termination to complete


def run_discord_bot():
    """Starts the Discord bot."""
    process = subprocess.Popen(
        [sys.executable, "base/bot.py"],
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    # Wait for the subprocess to complete, but handle the interrupt gracefully.
    try:
        process.wait()
    except KeyboardInterrupt:
        print("Discord bot process interrupted, terminating bot.")
        process.terminate()
        process.wait()  # Wait for termination to complete


if __name__ == "__main__":
    # Set the start method to 'spawn' for better cross-platform compatibility
    # and to avoid issues with inherited resources.
    multiprocessing.set_start_method("spawn", force=True)

    api_process = multiprocessing.Process(target=run_fastapi)
    bot_process = multiprocessing.Process(target=run_discord_bot)

    api_process.start()
    bot_process.start()

    try:
        api_process.join()
        bot_process.join()
    except KeyboardInterrupt:
        print("\n--- Shutting down services ---")

        if api_process.pid:
            os.kill(api_process.pid, signal.SIGINT)
        if bot_process.pid:
            os.kill(bot_process.pid, signal.SIGINT)

        api_process.join(timeout=5)
        bot_process.join(timeout=5)

        if api_process.is_alive():
            api_process.terminate()
        if bot_process.is_alive():
            bot_process.terminate()

        print("--- Services stopped ---")
