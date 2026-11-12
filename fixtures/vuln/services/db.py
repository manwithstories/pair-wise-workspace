import os


def run_command(command):
    return os.system(command)


def execute_query(cursor, sql):
    return cursor.execute(sql)
