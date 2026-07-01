#!/usr/bin/env python3
"""Post-process the swag-generated OpenAPI spec.

swag v2.0.0-rc5 (the latest release) emits every requestBody schema as a bogus
    {"oneOf": [{"type": "object"}, {"$ref": ".../X", "summary": ..., "description": ...}]}
The leading {"type": "object"} alternative validates ANY object, so Scalar,
Redoc, widdershins, and codegen all short-circuit on it and render the request
body as an empty "{}" with no fields. This collapses that exact shape back to a
plain {"$ref": ".../X"} so the real schema (with its documented fields) is used.

Runs in the generate pipeline AFTER swag and BEFORE scripts/gen-api-docs.sh
(see the //go:generate directives in main.go). Idempotent; rewrites both
swagger.json and swagger.yaml so the embedded + served specs match.
"""

import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OAPI = os.path.join(ROOT, "internal", "server", "oapi")
JSON_PATH = os.path.join(OAPI, "swagger.json")
YAML_PATH = os.path.join(OAPI, "swagger.yaml")


def is_bogus_oneof(schema):
    """True if schema is the swag {oneOf: [{type:object}, {$ref}]} artifact."""
    if not isinstance(schema, dict) or set(schema.keys()) != {"oneOf"}:
        return None
    alts = schema["oneOf"]
    if not isinstance(alts, list) or len(alts) != 2:
        return None
    bare = [a for a in alts if a == {"type": "object"}]
    refs = [a for a in alts if isinstance(a, dict) and "$ref" in a]
    if len(bare) == 1 and len(refs) == 1:
        return refs[0]["$ref"]
    return None


def fix_json(text):
    doc = json.loads(text)
    count = 0
    for path, ops in (doc.get("paths") or {}).items():
        if not isinstance(ops, dict):
            continue
        for method, op in ops.items():
            if not isinstance(op, dict):
                continue
            content = ((op.get("requestBody") or {}).get("content")) or {}
            for media in content.values():
                schema = media.get("schema") if isinstance(media, dict) else None
                ref = is_bogus_oneof(schema)
                if ref is not None:
                    media["schema"] = {"$ref": ref}
                    count += 1
    out = json.dumps(doc, indent=4, ensure_ascii=False)
    # Match Go's encoding/json HTML escaping so the diff stays minimal.
    out = out.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026")
    if text.endswith("\n") and not out.endswith("\n"):
        out += "\n"
    return out, count


def fix_yaml(text):
    """Collapse the same bogus oneOf shape in the YAML rendering.

    The block is machine-emitted by swag with a fixed shape, e.g.:
        <indent>oneOf:
        <indent>- type: object
        <indent>- $ref: '#/components/schemas/server.createJobRequest'
        <indent>  description: job request
        <indent>  summary: request
    -> <indent>$ref: '#/components/schemas/server.createJobRequest'
    """
    lines = text.split("\n")
    out = []
    i, n, count = 0, len(lines), 0
    while i < n:
        line = lines[i]
        m = re.match(r"^(\s*)oneOf:\s*$", line)
        if (m and i + 2 < n
                and lines[i + 1] == m.group(1) + "- type: object"
                and lines[i + 2].startswith(m.group(1) + "- $ref:")):
            indent = m.group(1)
            ref = lines[i + 2][len(indent + "- $ref:"):].strip()
            out.append(indent + "$ref: " + ref)
            j = i + 3
            # Drop the $ref's sibling keys (description/summary), indented deeper.
            while j < n and lines[j].strip() != "" and (len(lines[j]) - len(lines[j].lstrip(" "))) > len(indent):
                j += 1
            i, count = j, count + 1
            continue
        out.append(line)
        i += 1
    return "\n".join(out), count


def main():
    with open(JSON_PATH, encoding="utf-8") as f:
        jtext = f.read()
    jout, jn = fix_json(jtext)
    with open(JSON_PATH, "w", encoding="utf-8") as f:
        f.write(jout)

    yn = 0
    if os.path.exists(YAML_PATH):
        with open(YAML_PATH, encoding="utf-8") as f:
            ytext = f.read()
        yout, yn = fix_yaml(ytext)
        with open(YAML_PATH, "w", encoding="utf-8") as f:
            f.write(yout)

    print(f"fix-openapi: collapsed {jn} oneOf requestBody schema(s) in swagger.json, {yn} in swagger.yaml")
    if jn == 0:
        # Not fatal (idempotent re-runs legitimately find none), but worth noting.
        print("fix-openapi: note: no bogus oneOf found (already clean, or swag output changed)", file=sys.stderr)


if __name__ == "__main__":
    main()
