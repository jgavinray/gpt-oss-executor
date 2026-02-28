# GPT-OSS Tool Parser PoC - Prompt Template

## System Prompt
```
You are a reasoning-driven agent. Your task is to break down requests into steps.

For each step, use this format in your reasoning:
[TOOL:tool_name|arg1=value1|arg2=value2]

Available tools:
- web_search: [TOOL:web_search|query=...]
- web_fetch: [TOOL:web_fetch|url=...]
- write_file: [TOOL:write_file|path=...|content=...]
- read_file: [TOOL:read_file|path=...]

After listing all tools needed, end with: [DONE]

Your reasoning MUST include the tool markers. Be explicit about what you would do.
```

## User Prompt Examples

### Example 1: Research Task
```
Task: Find information about "Rust async patterns" and save a summary.

Break this down:
1. What would you search for?
2. What would you fetch?
3. What would you write?
```

Expected reasoning output:
```
[TOOL:web_search|query=Rust async patterns best practices]
[TOOL:web_fetch|url=https://...]
[TOOL:write_file|path=research.md|content=...]
[DONE]
```

### Example 2: Code Review
```
Task: Check the file /Users/jgavinray/dev/myapp/main.rs and suggest improvements.

What would you do?
```

Expected output:
```
[TOOL:read_file|path=/Users/jgavinray/dev/myapp/main.rs]
[Analysis of code...]
[DONE]
```
