#!/usr/bin/env python3

import sys
from datetime import datetime, timedelta, timezone

import jwt
import requests
from pkg_resources import packaging

parse_version = packaging.version.parse

if parse_version(jwt.__version__) < parse_version("2"):
    raise RuntimeError("PyJWT >= 2.0.0 is required")

def main():
    now = datetime.now(timezone.utc)
    claim = jwt.encode(
        {
            "iat": now - timedelta(seconds=10),
            "exp": now + timedelta(minutes=9, seconds=50),
            "iss": "49039",
        }, sys.stdin.read(), algorithm="RS256",
    )
    response = requests.post(
        f"https://api.github.com/app/installations/{sys.argv[1]}/access_tokens",
        headers={
            "Authorization": f"Bearer {claim}",
            "Accept": "application/vnd.github.v3+json",
        },
    )
    data = response.json()
    if "token" in data:
        print(data["token"])
        return 0
    print(data, file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
