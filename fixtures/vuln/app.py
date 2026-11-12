from handlers import route_a, route_b
from services import fetch as load_env
from services.input import fetch_user
from services.middle import relay as forward


def main(request):
    route_a(request)
    route_b(request)
    load_env()
    returned = fetch_user(request)
    forward(returned)
