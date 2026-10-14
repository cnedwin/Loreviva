# Contributing to Loreviva

You are welcome to adapt world settings, characters, Agents and Skills, and improve integration methods.

The current public code is a development foundation that has been checked. See the [README](README.md) for the implemented scope. Check existing Issues first. For feature suggestions, describe the specific problem a creator or integrating party encounters, then propose the smallest practical change.

## Submitting changes

1. Fork the repository and develop on a separate branch.
2. Prepare your environment according to [Setup and Integration](04-setup-and-integration.md).
3. Add tests that verify behavior and run the relevant checks. When a database is involved, use an isolated synthetic database.
4. Create a Pull Request describing the changed behavior, verification results and remaining limitations. Do not submit secret keys, personal conversations, database dumps, caches or generated runtime data.

For interface changes, first update `contracts/source/`, run the generation tools, and commit the generated files that match the source. The program is responsible for facts, permissions and transactions. Agents produce candidate proposals; external text cannot directly acquire execution authority.

## License

Copyright 2026 Loreviva contributors.

Loreviva's original code uses [Apache-2.0](../../LICENSE). Submitting a contribution means that you have the right to provide it and agree to provide it under this license; copyright transfer is not required. When referring to third-party material, retain its original license, copyright and required NOTICE. Model services, model weights, creator content and community materials each remain subject to their own authorization.
