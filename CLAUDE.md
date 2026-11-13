# appdeploy

The design lives in [SPEC.md](SPEC.md).

## Language

The repository's language is English: all documentation, code, comments, commit
messages and CLI output are written in English, even when the conversation is in
another language.

## Testing

Test against small Go applications in `examples/` (see SPEC.md §22), not Rails apps.
Tests must clean up what they create: containers, images, volumes, build directories.
