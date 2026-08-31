# QoS tests

`scripts/inference-smoke-test` verifies the currently configured global limit:
three requests per minute for each distinct Keycloak `sub` on the LLM route.
It creates two users and verifies that exhausting one user's bucket does not
limit the other user.
