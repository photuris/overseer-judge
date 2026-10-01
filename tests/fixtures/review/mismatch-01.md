# Task 007, round 1

### R1-01: Limiter ignores the burst setting

Severity: blocking
Status: open

`src/limiter.py:41` reads `rate` and never reads `burst`.

Response (fixed, 3dfc1be): the limiter now reads `burst` from config.

### R1-02: No test covers a zero rate

- file: tests/test_limiter.py
- severity: minor
- status: open

A rate of 0 is not exercised by any test.

- response: evidence: tests/test_limiter.py:88 asserts the zero-rate
case raises `ValueError`.
