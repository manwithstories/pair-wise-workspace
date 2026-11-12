from flask import request
from services.middle import relay


def deep_flow():
    payload = request.json
    relay(payload)
