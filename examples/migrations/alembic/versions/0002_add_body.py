from alembic import op
import sqlalchemy as sa

revision = "0002"
down_revision = "0001"


def upgrade():
    op.add_column("posts", sa.Column("body", sa.Text()))


def downgrade():
    op.drop_column("posts", "body")
