import os

import psycopg2
from pgvector.psycopg2 import register_vector


def get_db_connection():
    """Establishes a connection to the PostgreSQL database."""
    try:
        conn = psycopg2.connect(
            dbname=os.getenv("DB_NAME"),
            user=os.getenv("DB_USER"),
            password=os.getenv("DB_PASSWORD"),
            host=os.getenv("DB_HOST", "localhost"),
            port=os.getenv("DB_PORT", "5432"),
        )
        register_vector(conn)
        return conn
    except psycopg2.OperationalError as e:
        print(
            f"Error: Could not connect to the database. Please check your connection settings. Details: {e}"
        )
        return None


def setup_database():
    """Sets up the database table if it doesn't exist."""
    conn = get_db_connection()
    if conn is None:
        return

    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA", "my_app_schema")
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
            print(f"Table '{schema_name}.chat_logs' is ready.")
        conn.commit()
    except Exception as e:
        print(f"An error occurred during database setup: {e}")
    finally:
        if conn:
            conn.close()


def save_chat(
    username: str, prompt: str, response: str, prompt_embedding, response_embedding
):
    """Saves a chat prompt, its response, the user, and their embeddings to the database."""
    conn = get_db_connection()
    if conn is None:
        print("Could not save chat log due to no database connection.")
        return

    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA", "my_app_schema")
            cur.execute(
                f"""
                INSERT INTO {schema_name}.chat_logs (username, prompt, response, prompt_embedding, response_embedding)
                VALUES (%s, %s, %s, %s, %s)
                """,
                (username, prompt, response, prompt_embedding, response_embedding),
            )
        conn.commit()
        print(f"Successfully saved chat log for user '{username}' to the database.")
    except Exception as e:
        print(f"An error occurred while saving the chat log: {e}")
    finally:
        if conn:
            conn.close()
