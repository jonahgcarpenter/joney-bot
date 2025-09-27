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
        log.debug("Database connection successful.")
        return conn
    except psycopg2.OperationalError as e:
        log.error(f"Could not connect to the database. Details: {e}")
        return None


def setup_database():
    """Sets up the database tables if they don't exist."""
    conn = get_db_connection()
    if conn is None:
        return

    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA")
            cur.execute(f"CREATE SCHEMA IF NOT EXISTS {schema_name};")

            # Create chat_logs table
            cur.execute(
                f"""
                CREATE TABLE IF NOT EXISTS {schema_name}.chat_logs (
                    id SERIAL PRIMARY KEY,
                    username TEXT NOT NULL,
                    prompt TEXT NOT NULL,
                    response TEXT NOT NULL,
                    prompt_embedding VECTOR(768),
                    response_embedding VECTOR(768),
                    search_queries TEXT,
                    created_at TIMESTAMPTZ DEFAULT NOW()
                );
            """
            )

            # Create users table for context
            cur.execute(
                f"""
                CREATE TABLE IF NOT EXISTS {schema_name}.users (
                    id SERIAL PRIMARY KEY,
                    username TEXT UNIQUE NOT NULL,
                    context TEXT,
                    created_at TIMESTAMPTZ DEFAULT NOW(),
                    updated_at TIMESTAMPTZ DEFAULT NOW()
                );
            """
            )

            # Create a trigger to automatically update the updated_at timestamp
            cur.execute(
                f"""
                CREATE OR REPLACE FUNCTION update_updated_at_column()
                RETURNS TRIGGER AS $$
                BEGIN
                   NEW.updated_at = now();
                   RETURN NEW;
                END;
                $$ language 'plpgsql';
                """
            )
            cur.execute(
                f"""
                DROP TRIGGER IF EXISTS update_users_updated_at ON {schema_name}.users;
                CREATE TRIGGER update_users_updated_at
                BEFORE UPDATE ON {schema_name}.users
                FOR EACH ROW
                EXECUTE FUNCTION update_updated_at_column();
                """
            )

            log.info(f"Database is ready")
        conn.commit()
    except Exception as e:
        log.error(f"An error occurred during database setup: {e}")
    finally:
        if conn:
            conn.close()


def get_user_context(username: str) -> str | None:
    """Retrieves the context for a given user."""
    conn = get_db_connection()
    if conn is None:
        return None

    context = None
    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA")
            cur.execute(
                f"SELECT context FROM {schema_name}.users WHERE username = %s",
                (username,),
            )
            result = cur.fetchone()
            if result:
                context = result[0]
                log.debug(f"Found context for user '{username}'.")
            else:
                log.debug(f"No context found for user '{username}'.")
    except Exception as e:
        log.error(f"Error retrieving context for user '{username}': {e}")
    finally:
        if conn:
            conn.close()
    return context


def get_recent_chats(username: str, limit: int) -> str:
    """Retrieves only the user's most recent prompts for analysis."""
    conn = get_db_connection()
    if conn is None:
        return ""

    user_prompts = []
    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA")
            cur.execute(
                f"""
                SELECT prompt FROM {schema_name}.chat_logs
                WHERE username = %s
                ORDER BY created_at DESC
                LIMIT %s;
                """,
                (username, limit),
            )
            # Fetch just the prompts and reverse for chronological order
            results = reversed(cur.fetchall())
            for row in results:
                user_prompts.append(row[0])
    except Exception as e:
        log.error(f"Error retrieving recent chats for user '{username}': {e}")
    finally:
        if conn:
            conn.close()

    # Join prompts into a single block of text for analysis
    return "\n".join(user_prompts)


def get_single_most_recent_chat(username: str) -> str | None:
    """Retrieves only the user's single most recent prompt for analysis."""
    conn = get_db_connection()
    if conn is None:
        return None

    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA")
            cur.execute(
                f"""
                SELECT prompt FROM {schema_name}.chat_logs
                WHERE username = %s
                ORDER BY created_at DESC
                LIMIT 1;
                """,
                (username,),
            )
            result = cur.fetchone()
            if result:
                # Return only the prompt text
                return result[0]
    except Exception as e:
        log.error(f"Error retrieving most recent chat for user '{username}': {e}")
    finally:
        if conn:
            conn.close()
    return None


def update_user_profile(username: str, profile: str):
    """Saves the AI-generated profile to the user's context."""
    conn = get_db_connection()
    if conn is None:
        log.error("Could not update user profile due to no database connection.")
        return

    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA")
            log.info(f"Updating profile for user '{username}'.")
            log.debug(f"New profile for '{username}': {profile}")

            # Use INSERT ... ON CONFLICT to create a new user or update an existing one
            cur.execute(
                f"""
                INSERT INTO {schema_name}.users (username, context)
                VALUES (%s, %s)
                ON CONFLICT (username)
                DO UPDATE SET context = EXCLUDED.context;
                """,
                (username, profile),
            )
        conn.commit()
    except Exception as e:
        log.error(f"Error updating profile for user '{username}': {e}")
    finally:
        if conn:
            conn.close()


def save_chat(
    username: str,
    prompt: str,
    response: str,
    prompt_embedding,
    response_embedding,
    search_queries: list[str] | None = None,
):
    """Saves a chat prompt, its response, the user, embeddings, and search queries to the database."""
    conn = get_db_connection()
    if conn is None:
        log.error("Could not save chat log due to no database connection.")
        return

    queries_str = ", ".join(search_queries) if search_queries else None

    try:
        with conn.cursor() as cur:
            schema_name = os.getenv("DB_SCHEMA")
            cur.execute(
                f"""
                INSERT INTO {schema_name}.chat_logs (username, prompt, response, prompt_embedding, response_embedding, search_queries)
                VALUES (%s, %s, %s, %s, %s, %s)
                """,
                (
                    username,
                    prompt,
                    response,
                    prompt_embedding,
                    response_embedding,
                    queries_str,
                ),
            )
        conn.commit()
        log.info(f"SUCCESS: Saved chat from '{username}'.")
    except Exception as e:
        log.error(f"An error occurred while saving the chat log for '{username}': {e}")
    finally:
        if conn:
            conn.close()
