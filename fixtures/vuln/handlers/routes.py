from flask import request
import os
from services.middle import relay, relay_query


def route_a(req):
    value = request.args.get("q")
    relay(value)
    relay_query(None, value)
    os.system(req)
    return req


def route_b(req):
    return relay_query(None, req)
