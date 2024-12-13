# ask

**ask** is a command-line interface (CLI) tool that integrates with OpenAI's ChatGPT API to streamline prompt creation, refinement, and context addition. It allows you to quickly generate commands, code snippets, or answers, and easily refine them interactively. It also supports managing and replaying session histories, adding context, and truncating content to avoid exceeding token limits.

## Features

- **Initial Prompting**:  
  Run `ask "your prompt"` or simply `ask` (opens an editor) to submit prompts to ChatGPT.  
  If no prompt is provided, an editor is launched for you to enter one, and you can add additional context before sending.

- **Context Management**:  
  Add context to your queries by using `ask context <command>` before running `ask`. This context is appended to the initial prompt, making it easier to provide logs, file listings, or other data.

- **Refinement**:  
  After receiving an answer, use `ask refine` to provide additional instructions or context that refines the previously returned answer. The tool automatically includes the original prompt, previous response, and any run output or context from the last session.

- **Interactive Mode**:  
  Run `ask interactive` to enter an interactive REPL-like environment:
  - Set or edit prompts with `prompt` or `prompt <text>`.
  - Use `ask` to send the current prompt to ChatGPT.
  - Refine the answer with `refine`.
  - Add context via `context` commands.
  - Extract and run commands found in the answer with `run`.
    - `run` lists all commands found.
    - `run N` runs the Nth command.
  
- **History and Sessions**:  
  Each prompt and response is stored in `~/.ask/sessions` with a timestamp, so you can review your history and outputs later.

- **Model and Token Configuration**:  
  Set the default model with `ask config set-model <MODEL>` and the max token limit with `ask config set-max-tokens <NUMBER>`. Override the model per-invocation with `-model`.

- **Context Length Handling**:  
  The tool approximates token usage and truncates long prompts to avoid exceeding model token limits, preventing errors and allowing smoother workflows.

- **Listing Models**:  
  Use `ask models` to list available models from the API, making it easier to discover and switch between them.

## Installation

1. Ensure you have Go installed.
2. Clone this repository:
   ```bash
   git clone https://github.com/yourusername/ask.git
   cd ask
   ```
3. Install dependencies and build:
   ```bash
   go mod tidy
   go build -o ask .
   ```
4. Set your `OPENAI_API_KEY` environment variable:
   ```bash
   export OPENAI_API_KEY=sk-...
   ```
   Or use `ask config set-key <YOUR_API_KEY>` to store it securely in the config file.

5. Optionally set a default model:
   ```bash
   ask config set-model gpt-4
   ask config set-max-tokens 4096
   ```

You're ready to use `ask`!

## Usage Examples

- **Basic Ask**:
  ```bash
  ask "Generate a command to list all .go files"
  ```
  
- **Refine the Last Response**:
  ```bash
  ask refine
  # Opens editor to provide refinement instructions
  ```
  
- **Add Context Before Asking**:
  ```bash
  ask context "ls -l"
  ask "Analyze the file structure above"
  ```

- **Interactive Mode**:
  ```bash
  ask interactive
  > prompt "I want to learn how to parse JSON in Python"
  > ask
  > refine
  > run      # to list commands found in the answer
  > run 1    # to run the first command
  ```

## Notes on Development

This project’s development included steps guided by artificial intelligence (AI) assistance. While the code and logic underwent human review and modification, the initial implementations and refinements were heavily influenced by suggestions from an AI model. Treat the code and logic as you would any open-source project—test, verify, and adapt it to your needs.

## License

[MIT License](LICENSE)  
Feel free to use, modify, and distribute this tool under the terms of the MIT license.
