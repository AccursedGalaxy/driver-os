# csvcut — specification

`csvcut N` reads CSV from stdin and writes the Nth field (1-based) of every
record to stdout, one field per record, each followed by `\n`.

Input is CSV per **RFC 4180**:

- Fields are separated by commas; records by newlines.
- A field may be enclosed in double quotes. Inside a quoted field:
  - commas are literal (they do NOT split the field),
  - a doubled double-quote `""` is an escaped literal `"`,
  - newlines are literal (a quoted field may span multiple input lines —
    the record continues until the closing quote).
- The field is emitted **decoded**: without the enclosing quotes and with
  `""` collapsed to `"`. Literal commas and newlines inside a quoted field
  are emitted as-is.

Exit status 0 on success. If a record has fewer than N fields, emit an empty
line for it.

## Example

Input:

    1,"Smith, John",ok

`csvcut 2` emits:

    Smith, John
