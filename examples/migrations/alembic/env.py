import os

from alembic import context
from sqlalchemy import create_engine
from sqlalchemy.engine import make_url

url = make_url(os.environ["DATABASE_URL"]).set(drivername="postgresql+psycopg")
engine = create_engine(url)

with engine.connect() as connection:
    context.configure(connection=connection)
    with context.begin_transaction():
        context.run_migrations()
