from alembic import op
import sqlalchemy as sa

revision = "0001"
down_revision = None


def upgrade():
    op.create_table("posts", sa.Column("id", sa.BigInteger(), primary_key=True))


def downgrade():
    op.drop_table("posts")
