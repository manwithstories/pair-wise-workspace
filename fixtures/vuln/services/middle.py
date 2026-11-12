from .db import execute_query, run_command


def relay(value):
    return run_command(value)


def relay_query(cursor, value):
    return execute_query(cursor, value)
