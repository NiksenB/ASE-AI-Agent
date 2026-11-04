# ASE-AI-Agent

For the ITU course Advanced Software Engineering, this repository contains the code for an AI Agent, following the guide by Thorsten Ball (https://ampcode.com/notes/how-to-build-an-agent).

The agent calls the Corti Models API (Anthropic-compatible endpoint, model `corti-s1`).
Set the bearer token, then run:

```sh
export ANTHROPIC_AUTH_TOKEN=<corti bearer token>
cd code-editing-agent && go run .
```

Use `ANTHROPIC_AUTH_TOKEN`, not `ANTHROPIC_API_KEY`. The SDK sends `ANTHROPIC_API_KEY`
in the `x-api-key` header; Corti accepts only `Authorization: Bearer`.
