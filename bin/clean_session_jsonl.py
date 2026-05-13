import argparse
import json
from pathlib import Path


TOP_LEVEL_FIELDS = (
    "record_type",
    "request_method",
    "is_stream",
    "origin_model_name",
    "upstream_model",
    "request_object",
    "request_body",
    "response_body",
    "response_text",
)

TOKEN_USAGE_FIELDS = {
    "prompt_tokens",
    "completion_tokens",
    "total_tokens",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Clean session JSONL records and keep only selected fields."
    )
    parser.add_argument("-in", dest="input_path", required=True, help="Input JSONL path")
    parser.add_argument("-out", dest="output_path", help="Output JSONL path")
    return parser.parse_args()


def default_output_path(input_path: Path) -> Path:
    if input_path.suffix:
        return input_path.with_name(f"{input_path.stem}.cleaned{input_path.suffix}")
    return input_path.with_name(f"{input_path.name}.cleaned.jsonl")


def is_token_usage_key(key: str) -> bool:
    return key in TOKEN_USAGE_FIELDS


def slim_request_headers(headers: object) -> dict[str, object] | None:
    if not isinstance(headers, dict):
        return None

    for key, value in headers.items():
        if key.lower() == "user-agent":
            return {"User-Agent": value}
    return None


def slim_response_usage(usage: object) -> dict[str, object] | None:
    if not isinstance(usage, dict):
        return None

    filtered = {key: value for key, value in usage.items() if is_token_usage_key(key)}
    return filtered or None


def get_user_agent(record: dict[str, object]) -> str:
    headers = record.get("request_headers")
    if not isinstance(headers, dict):
        return ""

    for key, value in headers.items():
        if key.lower() == "user-agent" and isinstance(value, str):
            return value.strip()
    return ""


def build_clean_record(record: dict[str, object]) -> dict[str, object]:
    cleaned: dict[str, object] = {}

    for field in TOP_LEVEL_FIELDS:
        if field in record:
            cleaned[field] = record[field]

    # Prefer structured request content when available; the raw JSON string is kept
    # only as a fallback for records without request_object.
    if "request_object" in cleaned and "request_body" in cleaned:
        del cleaned["request_body"]

    request_object = cleaned.get("request_object")
    if isinstance(request_object, dict) and "system" in request_object:
        request_object = dict(request_object)
        del request_object["system"]
        cleaned["request_object"] = request_object

    headers = slim_request_headers(record.get("request_headers"))
    if headers is not None:
        cleaned["request_headers"] = headers

    response_usage = slim_response_usage(record.get("response_usage"))
    if response_usage is not None:
        cleaned["response_usage"] = response_usage

    for key, value in record.items():
        if is_token_usage_key(key):
            cleaned[key] = value

    return cleaned


def should_keep(record: dict[str, object]) -> bool:
    user_agent = get_user_agent(record).lower()
    if user_agent.startswith("check-cx"):
        return False

    return (
        record.get("turn_complete") is True
        and record.get("response_http_status") == 200
    )


def clean_jsonl(input_path: Path, output_path: Path) -> tuple[int, int]:
    total = 0
    kept = 0

    with input_path.open("r", encoding="utf-8") as src, output_path.open(
        "w", encoding="utf-8", newline="\n"
    ) as dst:
        for line_number, line in enumerate(src, start=1):
            line = line.strip()
            if not line:
                continue

            total += 1
            try:
                record = json.loads(line)
            except json.JSONDecodeError as exc:
                print(f"skip line {line_number}: invalid json: {exc}")
                continue

            if not isinstance(record, dict) or not should_keep(record):
                continue

            cleaned = build_clean_record(record)
            dst.write(json.dumps(cleaned, ensure_ascii=False, separators=(",", ":")))
            dst.write("\n")
            kept += 1

    return total, kept


def main() -> None:
    args = parse_args()
    input_path = Path(args.input_path).expanduser().resolve()
    output_path = (
        Path(args.output_path).expanduser().resolve()
        if args.output_path
        else default_output_path(input_path)
    )

    total, kept = clean_jsonl(input_path, output_path)
    print(f"done: kept {kept}/{total} records -> {output_path}")


if __name__ == "__main__":
    main()
