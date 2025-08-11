import logging
import os

import psycopg2
from pgvector.psycopg2 import register_vector

log = logging.getLogger(__name__)


def get_db_connection():
    """Establishes a connection to the PostgreSQL database."""
    try:
        conn = psycopg2.connect(
            dbname=os.getenv("DB_NAME"),
            user=os.getenv("DB_USER"),
            password=os.getenv("DB_PASSWORD"),
            host=os.getenv("DB_HOST"),
            port=os.getenv("DB_PORT"),
        )
        register_vector(conn)
        return conn
    except psycopg2.OperationalError as e:
        log.error(f"Could not connect to the database. Details: {e}")
        return None


def setup_database():
    """Sets up the database table if it doesn't exist."""
    conn = get_db_connection()
    if conn is None:
        return

    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA")
            cur.execute(f"CREATE SCHEMA IF NOT EXISTS {schema_name};")

            cur.execute(
                f"""
                CREATE TABLE IF NOT EXISTS {schema_name}.chat_logs (
                    id SERIAL PRIMARY KEY,
                    username TEXT NOT NULL,
                    prompt TEXT NOT NULL,
                    response TEXT NOT NULL,
                    prompt_embedding VECTOR(768),
                    response_embedding VECTOR(768),
                    created_at TIMESTAMPTZ DEFAULT NOW()
                );
            """
            )
            log.info(f"Database is ready")
        conn.commit()
    except Exception as e:
        log.error(f"An error occurred during database setup: {e}")
    finally:
        if conn:
            conn.close()


def save_chat(
    username: str, prompt: str, response: str, prompt_embedding, response_embedding
):
    """Saves a chat prompt, its response, the user, and their embeddings to the database."""
    conn = get_db_connection()
    if conn is None:
        log.error("Could not save chat log due to no database connection.")
        return

    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA")
            cur.execute(
                f"""
                INSERT INTO {schema_name}.chat_logs (username, prompt, response, prompt_embedding, response_embedding)
                VALUES (%s, %s, %s, %s, %s)
                """,
                (username, prompt, response, prompt_embedding, response_embedding),
            )
        conn.commit()
        log.info(
            f"SUCCESS: Processed and saved chat from '{username}'. Prompt: \"{prompt[:75]}\""
        )
    except Exception as e:
        log.error(f"An error occurred while saving the chat log for '{username}': {e}")
    finally:
        if conn:
            conn.close()
