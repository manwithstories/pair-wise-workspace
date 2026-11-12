import os
from services.middle import relay


def fetch_user(query):
    return query


def fetch_env():
    value = os.getenv("CMD")
    relay(value)


def fetch_form(req):
    return req.form.get("x")
