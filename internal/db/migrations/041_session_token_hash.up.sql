-- Session tokens are stored hashed now; a plaintext row would never match.
DELETE FROM sessions;
