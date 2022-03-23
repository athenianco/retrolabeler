from datetime import datetime, timedelta, timezone
import sys

import jwt
import requests


def main():
    now = datetime.now(timezone.utc)
    claim = jwt.encode({
        "iat": now - timedelta(seconds=10),
        "exp": now + timedelta(minutes=9, seconds=50),
        "iss": "49039",
    }, sys.stdin.read(), algorithm="RS256").decode()
    response = requests.post(
        f"https://api.github.com/app/installations/{sys.argv[1]}/access_tokens",
        headers={
            "Authorization": f"Bearer {claim}",
            "Accept": "application/vnd.github.v3+json",
        }
    )
    data = response.json()
    if "token" in data:
        print(data["token"])
        return 0
    print(data, file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
