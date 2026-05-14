import argparse
import hashlib
import json
from pathlib import Path


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Clean session JSONL records into the minimal session_turn schema."
    )
    parser.add_argument("-in", dest="input_path", required=True, help="Input JSONL path")
    parser.add_argument("-out", dest="output_path", help="Output JSONL path")
    return parser.parse_args()


def default_output_path(input_path: Path) -> Path:
    if input_path.suffix:
        return input_path.with_name(f"{input_path.stem}.cleaned{input_path.suffix}")
    return input_path.with_name(f"{input_path.name}.cleaned.jsonl")


def first_non_empty(*values: object) -> str:
    for value in values:
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def first_non_none(*values: object) -> object:
    for value in values:
        if value is not None:
            return value
    return None


def get_user_agent(record: dict[str, object]) -> str:
    if isinstance(record.get("user_agent"), str):
        return record["user_agent"]
    headers = record.get("request_headers")
    if not isinstance(headers, dict):
        return ""
    for key, value in headers.items():
        if key.lower() == "user-agent" and isinstance(value, str):
            return value
    return ""


def slim_response_usage(usage: object, record: dict[str, object]) -> dict[str, object] | None:
    if isinstance(usage, dict):
        input_tokens = int_value(first_non_none(usage.get("input_tokens"), usage.get("prompt_tokens")))
        output_tokens = int_value(first_non_none(usage.get("output_tokens"), usage.get("completion_tokens")))
        total_tokens = int_value(usage.get("total_tokens"))
    else:
        input_tokens = int_value(first_non_none(record.get("input_tokens"), record.get("prompt_tokens")))
        output_tokens = int_value(first_non_none(record.get("output_tokens"), record.get("completion_tokens")))
        total_tokens = int_value(record.get("total_tokens"))
    if total_tokens == 0:
        total_tokens = input_tokens + output_tokens
    if input_tokens == 0 and output_tokens == 0 and total_tokens == 0:
        return None
    return {
        "input_tokens": input_tokens,
        "output_tokens": output_tokens,
        "total_tokens": total_tokens,
    }


def int_value(value: object) -> int:
    if isinstance(value, bool) or value is None:
        return 0
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value)
    if isinstance(value, str):
        try:
            return int(float(value.strip()))
        except ValueError:
            return 0
    return 0


def normalize_tool_input(value: object) -> dict[str, object]:
    if value is None:
        return {}
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return {}
        try:
            parsed = json.loads(text)
        except json.JSONDecodeError:
            return {"arguments": value}
        if isinstance(parsed, dict):
            return parsed
        return {"value": parsed}
    return {"value": value}


def text_content(value: object) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "".join(text_content(item) for item in value)
    if isinstance(value, dict):
        return first_non_empty(value.get("text"), value.get("content"))
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def tool_result_content(value: object) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def normalize_function_call_block(item: dict[str, object]) -> dict[str, object] | None:
    function = item.get("function") if isinstance(item.get("function"), dict) else {}
    name = first_non_empty(item.get("name"), function.get("name"))
    if not name:
        return None
    tool_id = first_non_empty(item.get("id"), item.get("call_id")) or f"toolu_{stable_hash(item)}"
    arguments = first_non_none(function.get("arguments"), item.get("arguments"), item.get("input"))
    return {
        "type": "tool_use",
        "id": tool_id,
        "name": name,
        "input": normalize_tool_input(arguments),
    }


def normalize_function_call_output_block(item: dict[str, object]) -> dict[str, object] | None:
    tool_use_id = first_non_empty(item.get("call_id"), item.get("tool_call_id"), item.get("id"))
    if not tool_use_id:
        return None
    block: dict[str, object] = {
        "type": "tool_result",
        "tool_use_id": tool_use_id,
        "content": tool_result_content(first_non_none(item.get("output"), item.get("content"))),
    }
    if isinstance(item.get("is_error"), bool):
        block["is_error"] = item["is_error"]
    else:
        block["is_error"] = False
    return block


def normalize_content_block(item: object) -> dict[str, object] | None:
    if isinstance(item, str):
        return {"type": "text", "text": item} if item else None
    if not isinstance(item, dict):
        text = tool_result_content(item)
        return {"type": "text", "text": text} if text else None

    block_type = first_non_empty(item.get("type"))
    if block_type in {"input_text", "output_text"}:
        block_type = "text"
    if block_type in {"thinking", "reasoning", "reasoning_content", "image", "document"}:
        return None
    if block_type == "function_call":
        return normalize_function_call_block(item)
    if block_type == "function_call_output":
        return normalize_function_call_output_block(item)
    if not block_type:
        if item.get("tool_use_id") is not None or item.get("tool_call_id") is not None:
            block_type = "tool_result"
        elif item.get("id") is not None and item.get("name") is not None:
            block_type = "tool_use"
        else:
            block_type = "text"

    if block_type == "text":
        text = first_non_empty(item.get("text"), item.get("content"))
        return {"type": "text", "text": text} if text else None
    if block_type == "tool_use":
        tool_id = first_non_empty(item.get("id"), item.get("call_id"))
        name = first_non_empty(item.get("name"))
        if not tool_id or not name:
            return None
        return {
            "type": "tool_use",
            "id": tool_id,
            "name": name,
            "input": normalize_tool_input(item.get("input")),
        }
    if block_type == "tool_result":
        tool_use_id = first_non_empty(
            item.get("tool_use_id"),
            item.get("tool_call_id"),
            item.get("call_id"),
            item.get("id"),
        )
        if not tool_use_id:
            return None
        return {
            "type": "tool_result",
            "tool_use_id": tool_use_id,
            "content": tool_result_content(item.get("content")),
            "is_error": item["is_error"] if isinstance(item.get("is_error"), bool) else False,
        }
    return None


def normalize_content_blocks(content: object) -> list[dict[str, object]]:
    if content is None:
        return []
    if isinstance(content, list):
        blocks = []
        for item in content:
            block = normalize_content_block(item)
            if block is not None:
                blocks.append(block)
        return blocks
    block = normalize_content_block(content)
    return [block] if block is not None else []


def normalize_tool_calls(value: object) -> list[dict[str, object]]:
    if not isinstance(value, list):
        return []
    blocks = []
    for item in value:
        if isinstance(item, dict):
            block = normalize_function_call_block(item)
            if block is not None:
                blocks.append(block)
    return blocks


def normalize_tool_result_message(message: dict[str, object]) -> dict[str, object] | None:
    tool_use_id = first_non_empty(
        message.get("tool_use_id"),
        message.get("tool_call_id"),
        message.get("call_id"),
        message.get("id"),
    )
    if not tool_use_id:
        return None
    return {
        "role": "user",
        "content": [
            {
                "type": "tool_result",
                "tool_use_id": tool_use_id,
                "content": tool_result_content(message.get("content")),
                "is_error": message.get("is_error") if isinstance(message.get("is_error"), bool) else False,
            }
        ],
    }


def normalize_messages(value: object) -> list[dict[str, object]]:
    if not isinstance(value, list):
        return []
    messages = []
    for item in value:
        if not isinstance(item, dict):
            continue
        role = first_non_empty(item.get("role"))
        if role == "tool" or item.get("tool_call_id") is not None or item.get("tool_use_id") is not None:
            message = normalize_tool_result_message(item)
            if message is not None:
                messages.append(message)
            continue
        content = normalize_content_blocks(item.get("content"))
        content.extend(normalize_tool_calls(item.get("tool_calls")))
        if not content:
            continue
        messages.append({"role": "assistant" if role == "assistant" else "user", "content": content})
    return messages


def normalize_tools(value: object) -> list[dict[str, object]]:
    if not isinstance(value, list):
        return []
    tools = []
    for item in value:
        if not isinstance(item, dict):
            continue
        function = item.get("function") if isinstance(item.get("function"), dict) else {}
        name = first_non_empty(item.get("name"), function.get("name"))
        if not name:
            continue
        tool: dict[str, object] = {"name": name}
        description = first_non_empty(item.get("description"), function.get("description"))
        if description:
            tool["description"] = description
        input_schema = first_non_none(
            item.get("input_schema"),
            item.get("inputSchema"),
            item.get("parameters"),
            function.get("parameters"),
        )
        if isinstance(input_schema, dict):
            tool["input_schema"] = input_schema
        tools.append(tool)
    return tools


def normalize_request_object(value: object) -> dict[str, object]:
    request_object: dict[str, object] = {"messages": []}
    if not isinstance(value, dict):
        return request_object
    if "messages" in value:
        request_object["messages"] = normalize_messages(value.get("messages"))
    elif "input" in value:
        request_object["messages"] = normalize_input_messages(value.get("input"))
    tools = normalize_tools(value.get("tools"))
    if not tools:
        tools = normalize_tools(value.get("functions"))
    if tools:
        request_object["tools"] = tools
    request_object = filter_orphan_tool_results(ensure_tool_definitions(request_object))
    return request_object


def append_response_output(request_object: dict[str, object], record: dict[str, object]) -> dict[str, object]:
    content = []
    response_text = first_non_empty(record.get("response_text"), extract_response_text(record.get("response_body")))
    if response_text:
        content.append({"type": "text", "text": response_text})
    content.extend(extract_response_tool_calls(record.get("response_body")))
    if not content:
        return request_object
    messages = request_object.get("messages")
    if not isinstance(messages, list):
        messages = []
    if not assistant_output_already_present(messages, content):
        messages.append({"role": "assistant", "content": content})
    request_object["messages"] = messages
    return filter_orphan_tool_results(ensure_tool_definitions(request_object))


def assistant_output_already_present(messages: list[object], content: list[dict[str, object]]) -> bool:
    if not messages:
        return False
    last = messages[-1]
    if not isinstance(last, dict) or last.get("role") != "assistant":
        return False
    return last.get("content") == content


def response_json_fragments(body: object) -> list[object]:
    if not isinstance(body, str) or not body.strip():
        return []
    text = body.strip()
    fragments = []
    if "data:" in text:
        for line in text.splitlines():
            line = line.strip()
            if not line.startswith("data:"):
                continue
            payload = line.removeprefix("data:").strip()
            if not payload or payload == "[DONE]":
                continue
            fragments.append(payload)
    elif "\n" in text:
        fragments.extend(
            line.strip()
            for line in text.splitlines()
            if line.strip().startswith(("{", "["))
        )
    else:
        fragments.append(text)

    parsed = []
    for fragment in fragments:
        try:
            parsed.append(json.loads(fragment))
        except json.JSONDecodeError:
            continue
    return parsed


def extract_response_text(body: object) -> str:
    return "".join(text_from_payload(payload) for payload in response_json_fragments(body))


def text_from_payload(payload: object) -> str:
    if isinstance(payload, list):
        return "".join(text_from_payload(item) for item in payload)
    if not isinstance(payload, dict):
        return payload if isinstance(payload, str) else ""
    parts = []
    choices = payload.get("choices")
    if isinstance(choices, list):
        for choice in choices:
            if not isinstance(choice, dict):
                continue
            if isinstance(choice.get("text"), str):
                parts.append(choice["text"])
            message = choice.get("message")
            if isinstance(message, dict):
                parts.append(text_content(message.get("content")))
            delta = choice.get("delta")
            if isinstance(delta, dict):
                parts.append(text_content(delta.get("content")))
    outputs = payload.get("output")
    if isinstance(outputs, list):
        for output in outputs:
            if isinstance(output, dict):
                parts.append(text_content(output.get("content")))
                if isinstance(output.get("text"), str):
                    parts.append(output["text"])
    if payload.get("type") == "output_text" and isinstance(payload.get("text"), str):
        parts.append(payload["text"])
    parts.append(text_content(payload.get("content")))
    if isinstance(payload.get("text"), str):
        parts.append(payload["text"])
    delta = payload.get("delta")
    if isinstance(delta, dict):
        parts.append(text_content(delta.get("text")))
    return "".join(parts)


def extract_response_tool_calls(body: object) -> list[dict[str, object]]:
    blocks = []
    for payload in response_json_fragments(body):
        collect_response_tool_calls(payload, blocks)
    return blocks


def collect_response_tool_calls(payload: object, blocks: list[dict[str, object]]) -> None:
    if isinstance(payload, list):
        for item in payload:
            collect_response_tool_calls(item, blocks)
        return
    if not isinstance(payload, dict):
        return
    choices = payload.get("choices")
    if isinstance(choices, list):
        for choice in choices:
            if not isinstance(choice, dict):
                continue
            for key in ("message", "delta"):
                item = choice.get(key)
                if isinstance(item, dict):
                    tool_calls = item.get("tool_calls")
                    if isinstance(tool_calls, list):
                        for tool_call in tool_calls:
                            if isinstance(tool_call, dict):
                                block = normalize_function_call_block(tool_call)
                                if block is not None:
                                    blocks.append(block)
    outputs = payload.get("output")
    if isinstance(outputs, list):
        for output in outputs:
            if isinstance(output, dict) and output.get("type") == "function_call":
                block = normalize_function_call_block(
                    {
                        "id": output.get("id"),
                        "call_id": output.get("call_id"),
                        "name": output.get("name"),
                        "arguments": output.get("arguments"),
                    }
                )
                if block is not None:
                    blocks.append(block)
    content = payload.get("content")
    if isinstance(content, list):
        for item in content:
            if isinstance(item, dict) and item.get("type") == "tool_use":
                block = normalize_content_block(item)
                if block is not None:
                    blocks.append(block)


def normalize_input_messages(value: object) -> list[dict[str, object]]:
    if isinstance(value, str):
        return [{"role": "user", "content": [{"type": "text", "text": value}]}] if value else []
    if isinstance(value, list):
        messages = normalize_messages(value)
        if messages:
            return messages
        content = []
        for item in value:
            block = normalize_content_block(item)
            if block is not None:
                content.append(block)
        return [{"role": "user", "content": content}] if content else []
    if value is not None:
        text = tool_result_content(value)
        return [{"role": "user", "content": [{"type": "text", "text": text}]}] if text else []
    return []


def has_usable_context(request_object: dict[str, object]) -> bool:
    if request_object.get("tools"):
        return True
    messages = request_object.get("messages")
    if not isinstance(messages, list):
        return False
    for message in messages:
        if not isinstance(message, dict):
            continue
        content = message.get("content")
        if not isinstance(content, list) or not content:
            continue
        if message.get("role") == "user":
            return True
        for block in content:
            if isinstance(block, dict) and block.get("type") in {"tool_use", "tool_result"}:
                return True
    return False


def has_assistant_output(request_object: dict[str, object]) -> bool:
    messages = request_object.get("messages")
    if not isinstance(messages, list):
        return False
    for message in messages:
        if not isinstance(message, dict) or message.get("role") != "assistant":
            continue
        content = message.get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict):
                continue
            if block.get("type") == "text" and first_non_empty(block.get("text")):
                return True
            if block.get("type") == "tool_use" and first_non_empty(block.get("id"), block.get("name")):
                return True
    return False


def ensure_tool_definitions(request_object: dict[str, object]) -> dict[str, object]:
    names = []
    seen = set()
    messages = request_object.get("messages")
    if isinstance(messages, list):
        for message in messages:
            if not isinstance(message, dict) or message.get("role") != "assistant":
                continue
            content = message.get("content")
            if not isinstance(content, list):
                continue
            for block in content:
                if not isinstance(block, dict) or block.get("type") != "tool_use":
                    continue
                name = first_non_empty(block.get("name"))
                if name and name not in seen:
                    names.append(name)
                    seen.add(name)
    if not names:
        return request_object
    tools = request_object.get("tools")
    if not isinstance(tools, list):
        tools = []
    existing = {
        tool.get("name")
        for tool in tools
        if isinstance(tool, dict) and isinstance(tool.get("name"), str)
    }
    for name in names:
        if name not in existing:
            tools.append({"name": name})
            existing.add(name)
    request_object["tools"] = tools
    return request_object


def filter_orphan_tool_results(request_object: dict[str, object]) -> dict[str, object]:
    messages = request_object.get("messages")
    if not isinstance(messages, list):
        return request_object
    tool_use_ids = set()
    for message in messages:
        if not isinstance(message, dict) or message.get("role") != "assistant":
            continue
        content = message.get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if isinstance(block, dict) and block.get("type") == "tool_use":
                tool_id = first_non_empty(block.get("id"))
                if tool_id:
                    tool_use_ids.add(tool_id)
    if not tool_use_ids:
        return request_object

    filtered_messages = []
    for message in messages:
        if not isinstance(message, dict):
            continue
        content = message.get("content")
        if not isinstance(content, list):
            filtered_messages.append(message)
            continue
        filtered_content = []
        for block in content:
            if not isinstance(block, dict) or block.get("type") != "tool_result":
                filtered_content.append(block)
                continue
            if first_non_empty(block.get("tool_use_id")) in tool_use_ids:
                filtered_content.append(block)
        if filtered_content:
            filtered_message = dict(message)
            filtered_message["content"] = filtered_content
            filtered_messages.append(filtered_message)
    request_object["messages"] = filtered_messages
    return request_object


def build_clean_record(record: dict[str, object]) -> dict[str, object] | None:
    user_agent = get_user_agent(record)
    if not user_agent.strip() or user_agent.strip().lower().startswith("check-cx"):
        return None

    request_object = append_response_output(normalize_request_object(record.get("request_object")), record)
    if not has_usable_context(request_object) or not has_assistant_output(request_object):
        return None

    session_id = first_non_empty(record.get("session_id"), record.get("session_key"))
    if not session_id:
        session_id = f"ctx_{stable_hash(request_object)}"

    cleaned: dict[str, object] = {
        "record_type": "session_turn",
        "session_id": session_id,
        "user_agent": user_agent,
    }
    response_usage = slim_response_usage(record.get("response_usage"), record)
    if response_usage is not None:
        cleaned["response_usage"] = response_usage
    cleaned["request_object"] = request_object
    return cleaned


def should_keep(record: dict[str, object]) -> bool:
    if record.get("turn_complete") is not None and record.get("turn_complete") is not True:
        return False
    if record.get("stream_complete") is not None and record.get("stream_complete") is not True:
        return False
    status = int_value(record.get("response_http_status"))
    if status and status >= 400:
        return False
    return True


def stable_hash(value: object) -> str:
    data = json.dumps(value, ensure_ascii=False, sort_keys=True, default=str)
    return hashlib.sha256(data.encode("utf-8")).hexdigest()[:32]


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
            if cleaned is None:
                continue
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
