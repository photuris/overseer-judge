# Task 007, round 1

## R1-01 Limiter ignores the burst setting

Severity: blocking

`src/limiter.py:41` reads `rate` and never reads `burst`.

Implementer response: fixed in 3dfc1be.
