package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chzyer/readline"
	"github.com/sashabaranov/go-openai"
)

const (
	historyDirName     = ".ask/sessions"
	configFileName     = ".ask/config.json"
	pendingContextFile = ".ask/pending_context.txt"
)

var (
	apiKey        = ""
	editor        = os.Getenv("EDITOR")
	model         = "gpt-4" // Default model if none set in config
	debugMode     bool
	maxTokens     = 1000000 // default max tokens if not set by user
	charsPerToken = 4       // approximate chars per token
)

type Config struct {
	APIKey    string `json:"api_key"`
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"` // user-configurable max tokens
}

func main() {
	loadAPIKey()

	// Define subcommands
	refineCmd := flag.NewFlagSet("refine", flag.ExitOnError)
	interactiveCmd := flag.NewFlagSet("interactive", flag.ExitOnError)
	contextCmd := flag.NewFlagSet("context", flag.ExitOnError)
	configCmd := flag.NewFlagSet("config", flag.ExitOnError)
	modelsCmd := flag.NewFlagSet("models", flag.ExitOnError)

	var fileFlag string
	var runFlag bool
	var debugFlag bool
	var modelFlag string

	// Global flags for main command
	flag.StringVar(&fileFlag, "f", "", "file path containing prompt")
	flag.BoolVar(&runFlag, "run", false, "immediately run the resulting command if feasible")
	flag.BoolVar(&debugFlag, "debug", false, "enable debug output")
	flag.StringVar(&modelFlag, "model", "", "Override the OpenAI model to use (e.g., gpt-4, gpt-3.5-turbo)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: ask [options] [prompt]

If prompt is omitted, an editor is opened. You can add context before sending.

Subcommands:
  refine       Refine the last session's response with additional context.
  interactive  Enter an interactive mode.
  context      Add shell command output as context to the last or future session.
  config       Manage configuration (store API key, model, or max-tokens).
  models       List available models from the API.

Options:
`)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Examples:
  ask "How to list all files?"
  ask -run "Generate a command to list files"
  ask refine
  ask config set-key <YOUR_API_KEY>
  ask config set-model gpt-3.5-turbo
  ask config set-max-tokens 8192
  ask models

Use 'ask <subcommand> -h' for subcommand help.
`)
	}

	if len(os.Args) < 2 {
		// No subcommand, just run main ask logic
		flag.Parse()
		debugMode = debugFlag
		if modelFlag != "" {
			model = modelFlag
		}
		handleAsk("", fileFlag, runFlag)
		return
	}

	if os.Args[1] == "-h" || os.Args[1] == "--help" {
		flag.Usage()
		os.Exit(0)
	}

	switch os.Args[1] {
	case "refine":
		refineCmd.BoolVar(&debugFlag, "debug", false, "enable debug output")
		refineCmd.StringVar(&modelFlag, "model", "", "Override the OpenAI model to use")
		refineCmd.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage: ask refine [options] [refinement text]\n")
			refineCmd.PrintDefaults()
		}
		refineCmd.Parse(os.Args[2:])
		debugMode = debugFlag
		if modelFlag != "" {
			model = modelFlag
		}
		handleRefine(refineCmd.Args())

	case "interactive":
		interactiveCmd.BoolVar(&debugFlag, "debug", false, "enable debug output")
		interactiveCmd.StringVar(&modelFlag, "model", "", "Override the OpenAI model")
		interactiveCmd.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage: ask interactive [options]\n")
			interactiveCmd.PrintDefaults()
		}
		interactiveCmd.Parse(os.Args[2:])
		debugMode = debugFlag
		if modelFlag != "" {
			model = modelFlag
		}
		handleInteractive(interactiveCmd.Args())

	case "context":
		contextCmd.BoolVar(&debugFlag, "debug", false, "enable debug output")
		contextCmd.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage: ask context [options] <command>\n")
			contextCmd.PrintDefaults()
		}
		contextCmd.Parse(os.Args[2:])
		debugMode = debugFlag
		handleContext(contextCmd.Args())

	case "config":
		configCmd.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage:\n  ask config set-key <YOUR_API_KEY>\n  ask config set-model <MODEL>\n  ask config set-max-tokens <NUMBER>\n")
			configCmd.PrintDefaults()
		}
		configCmd.Parse(os.Args[2:])
		handleConfig(configCmd.Args())

	case "models":
		modelsCmd.BoolVar(&debugFlag, "debug", false, "enable debug output")
		modelsCmd.StringVar(&modelFlag, "model", "", "Override the OpenAI model")
		modelsCmd.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage: ask models [options]\n")
			modelsCmd.PrintDefaults()
		}
		modelsCmd.Parse(os.Args[2:])
		debugMode = debugFlag
		if modelFlag != "" {
			model = modelFlag
		}
		handleModels()

	default:
		// Treat as main ask command with prompt
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		flag.CommandLine.StringVar(&fileFlag, "f", "", "file path containing prompt")
		flag.CommandLine.BoolVar(&runFlag, "run", false, "immediately run the resulting command if feasible")
		flag.CommandLine.BoolVar(&debugFlag, "debug", false, "enable debug output")
		flag.CommandLine.StringVar(&modelFlag, "model", "", "Override the OpenAI model")
		flag.CommandLine.Usage = flag.Usage
		flag.CommandLine.Parse(os.Args[1:])

		debugMode = debugFlag
		if modelFlag != "" {
			model = modelFlag
		}
		args := flag.CommandLine.Args()
		var prompt string
		if len(args) > 0 {
			prompt = strings.Join(args, " ")
		}
		handleAsk(prompt, fileFlag, runFlag)
	}
}

func loadAPIKey() {
	cfg, err := loadConfig()
	if err == nil && cfg != nil {
		if cfg.APIKey != "" {
			apiKey = decodeBase64(cfg.APIKey)
		}
		if cfg.Model != "" {
			model = cfg.Model // load default model from config
		}
		if cfg.MaxTokens > 0 {
			maxTokens = cfg.MaxTokens
		}
	} else if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] No valid config found or error loading config: %v\n", err)
	}

	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(os.Stderr, "No API key found. Set OPENAI_API_KEY or run `ask config set-key <YOUR_API_KEY>`.")
			os.Exit(1)
		}
	}

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Loaded API key, model=%s, max_tokens=%d\n", model, maxTokens)
	}
}

func loadConfig() (*Config, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(homedir, configFileName)
	data, err := ioutil.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var cfg Config
	err = json.Unmarshal(data, &cfg)
	return &cfg, err
}

func saveConfig(cfg *Config) error {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfgDir := filepath.Join(homedir, ".ask")
	err = os.MkdirAll(cfgDir, 0755)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(cfgDir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(cfgPath, data, 0600)
}

func handleConfig(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage:")
		fmt.Println("  ask config set-key <API_KEY>")
		fmt.Println("  ask config set-model <MODEL>")
		fmt.Println("  ask config set-max-tokens <NUMBER>")
		return
	}
	switch args[0] {
	case "set-key":
		if len(args) < 2 {
			fmt.Println("Usage: ask config set-key <API_KEY>")
			return
		}
		key := args[1]
		enc := base64.StdEncoding.EncodeToString([]byte(key))
		cfg, _ := loadConfig()
		if cfg == nil {
			cfg = &Config{}
		}
		cfg.APIKey = enc
		err := saveConfig(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("API Key saved to config.")
	case "set-model":
		if len(args) < 2 {
			fmt.Println("Usage: ask config set-model <MODEL>")
			return
		}
		modelName := args[1]

		// We won't validate here now, since we removed validateModel snippet
		cfg, _ := loadConfig()
		if cfg == nil {
			cfg = &Config{}
		}
		cfg.Model = modelName
		err := saveConfig(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Model '%s' saved to config.\n", modelName)
	case "set-max-tokens":
		if len(args) < 2 {
			fmt.Println("Usage: ask config set-max-tokens <NUMBER>")
			return
		}
		val, err := strconv.Atoi(args[1])
		if err != nil || val <= 0 {
			fmt.Println("Invalid number for max-tokens.")
			return
		}
		cfg, _ := loadConfig()
		if cfg == nil {
			cfg = &Config{}
		}
		cfg.MaxTokens = val
		err = saveConfig(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Max tokens '%d' saved to config.\n", val)
	default:
		fmt.Println("Unknown config command. Available: set-key, set-model, set-max-tokens")
	}
}

func decodeBase64(encoded string) string {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	return string(decoded)
}

func handleAsk(prompt, filePath string, run bool) {
	if prompt == "" && filePath != "" {
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to read file: %v\n", err)
			os.Exit(1)
		}
		prompt = string(data)
	} else if prompt == "" && filePath == "" {
		edited, err := openEditor("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open editor: %v\n", err)
			os.Exit(1)
		}
		prompt = edited
		prompt = runInitialContextLoop(prompt)
	}

	pending := loadPendingContext()
	if pending != "" {
		prompt += "\n\nAdditional Context:\n" + pending
		clearPendingContext()
	}

	if prompt == "" {
		fmt.Fprintln(os.Stderr, "No prompt provided.")
		os.Exit(1)
	}

	// Apply length limit based on max_tokens
	maxChars := maxTokens * charsPerToken
	if len(prompt) > maxChars {
		// Truncate prompt itself if needed
		prompt = prompt[:maxChars]
	} else {
		// If prompt + context are too long, try truncating context portion
		// Actually, we've already combined the prompt and context into `prompt`
		// So we can attempt a smarter truncation here:
		// We'll look for the "Additional Context:\n" marker and try truncating from there.
		index := strings.Index(prompt, "Additional Context:\n")
		if index > -1 {
			// If overall too long, truncate from end of the prompt
			if len(prompt) > maxChars {
				overage := len(prompt) - maxChars
				// Truncate from the end of context
				contextStart := index + len("Additional Context:\n")
				contextLen := len(prompt) - contextStart
				if contextLen > overage {
					// Just remove overage from context end
					newContextEnd := len(prompt) - overage
					prompt = prompt[:newContextEnd]
				} else {
					// Overage bigger than entire context, remove context entirely
					prompt = prompt[:index]
				}
			}
		} else {
			// No additional context marker and still too long?
			if len(prompt) > maxChars {
				prompt = prompt[:maxChars]
			}
		}
	}

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Asking prompt (len=%d, maxChars=%d):\n%s\n", len(prompt), maxChars, prompt)
	}

	answer, err := askChatGPT(prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting response: %v\n", err)
		os.Exit(1)
	}

	sessionPath, err := storeSession(prompt, answer, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Could not store session: %v\n", err)
	}

	fmt.Println(answer)

	if run {
		cmdStr := extractCommand(answer)
		if cmdStr != "" {
			if err := runCommandInteractively(cmdStr, sessionPath); err != nil {
				fmt.Fprintf(os.Stderr, "Error running command: %v\n", err)
			}
		} else {
			fmt.Fprintln(os.Stderr, "No runnable command found in the answer.")
		}
	} else {
		fmt.Fprintf(os.Stderr, "Session stored in: %s\n", sessionPath)
	}
}

func runInitialContextLoop(initialPrompt string) string {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:      "(context mode) > ",
		HistoryFile: filepath.Join(os.TempDir(), "ask_temp_history.txt"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing line editor: %v\n", err)
		return initialPrompt
	}
	defer rl.Close()

	fmt.Println("You may now add context or edit the prompt before finalizing.")
	fmt.Println("Commands:")
	fmt.Println(":context <cmd> - Run a shell command and add its output as context")
	fmt.Println(":edit          - Re-edit the prompt")
	fmt.Println(":done          - Finalize and send the prompt to ChatGPT")
	fmt.Println("(Use up/down arrows to cycle through history)")

	var extraContext strings.Builder
	prompt := initialPrompt

	for {
		line, err := rl.Readline()
		if err == readline.ErrInterrupt || err == io.EOF {
			return prompt
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == ":done" {
			if extraContext.Len() > 0 {
				prompt += "\n\nAdditional Context:\n" + extraContext.String()
			}
			return prompt
		} else if strings.HasPrefix(line, ":context ") {
			cmdStr := strings.TrimPrefix(line, ":context ")
			if debugMode {
				fmt.Fprintf(os.Stderr, "[DEBUG] Running context command: %s\n", cmdStr)
			}
			output, cerr := runShellCommand(cmdStr)
			if cerr != nil {
				fmt.Fprintf(os.Stderr, "Error adding context: %v\n", cerr)
			} else {
				fmt.Println(output)
				extraContext.WriteString("\n---\nCommand: " + cmdStr + "\n" + output + "\n")
			}
		} else if line == ":edit" {
			edited, err := openEditor(prompt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to open editor: %v\n", err)
			} else {
				prompt = edited
			}
		} else {
			fmt.Println("Unknown command. Available: :context <cmd>, :edit, :done")
		}
	}
}

func handleRefine(args []string) {
	lastPrompt, lastResponse, lastSessionPath, err := getLastSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error retrieving last session: %v\n", err)
		os.Exit(1)
	}

	originalPrompt, _ := ioutil.ReadFile(filepath.Join(lastSessionPath, "original_prompt.txt"))
	if len(originalPrompt) == 0 {
		originalPrompt = []byte(lastPrompt)
	}

	runOutput := readFileIfExists(filepath.Join(lastSessionPath, "run_output.txt"))
	contextOutput := readFileIfExists(filepath.Join(lastSessionPath, "context.txt"))

	refinement := ""
	if len(args) > 0 {
		refinement = strings.Join(args, " ")
	} else {
		initialRefine := "Provide refinement/context below:\n\n---\nPrevious Response:\n" + lastResponse
		if runOutput != "" {
			initialRefine += "\n\nRun Output:\n" + runOutput
		}
		if contextOutput != "" {
			initialRefine += "\n\nAdditional Context:\n" + contextOutput
		}
		initialRefine += "\n\nOriginal Prompt:\n" + string(originalPrompt)

		edited, err := openEditor(initialRefine)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open editor: %v\n", err)
			os.Exit(1)
		}
		refinement = edited
	}

	finalPrompt := "Refine the following response with the additional context:\n\nORIGINAL PROMPT:\n" + string(originalPrompt) +
		"\n\nPREVIOUS PROMPT:\n" + lastPrompt +
		"\n\nPREVIOUS RESPONSE:\n" + lastResponse

	if runOutput != "" {
		finalPrompt += "\n\nPREVIOUS COMMAND RUN OUTPUT:\n" + runOutput
	}
	if contextOutput != "" {
		finalPrompt += "\n\nADDITIONAL CONTEXT:\n" + contextOutput
	}
	finalPrompt += "\n\nREFINEMENT CONTEXT:\n" + refinement

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Refine finalPrompt:\n%s\n", finalPrompt)
	}

	// Token limit check again (in refinement)
	maxChars := maxTokens * charsPerToken
	if len(finalPrompt) > maxChars {
		finalPrompt = finalPrompt[:maxChars]
	}

	answer, err := askChatGPT(finalPrompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting refinement: %v\n", err)
		os.Exit(1)
	}

	sessionPath, err := storeSession(finalPrompt, answer, string(originalPrompt))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Could not store session: %v\n", err)
	}

	fmt.Println(answer)
	fmt.Fprintf(os.Stderr, "Refined session stored in: %s\n", sessionPath)
}

func handleInteractive(args []string) {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:      "> ",
		HistoryFile: filepath.Join(os.TempDir(), "ask_interactive_history.txt"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing line editor: %v\n", err)
		return
	}
	defer rl.Close()

	fmt.Println("Entering interactive mode. Type 'help' for commands, 'exit' to quit.")

	var currentPrompt string
	var currentAnswer string
	var currentSessionPath string
	var originalPrompt string
	var pendingContext strings.Builder

	// currentCommands holds all extracted commands from the current answer
	var currentCommands []string

	for {
		line, err := rl.Readline()
		if err != nil && err == io.EOF {
			break
		} else if err == readline.ErrInterrupt {
			break
		}

		line = strings.TrimSpace(line)

		switch {
		case line == "exit":
			return
		case line == "help":
			fmt.Println("Commands:")
			fmt.Println("  prompt           : Edit the current prompt in an editor")
			fmt.Println("  prompt <text>    : Set the current prompt directly to <text>")
			fmt.Println("  ask              : Submit the current prompt to ChatGPT")
			fmt.Println("  refine           : Refine the current answer with additional context")
			fmt.Println("  run              : List available commands extracted from the current answer")
			fmt.Println("  run <N>          : Run the Nth command (1-based) from the extracted commands")
			fmt.Println("  context          : Prompt for a command to add context")
			fmt.Println("  context <cmd>    : Run <cmd> and add output as context immediately")
			fmt.Println("  show             : Show current prompt and answer")
			fmt.Println("  exit             : Quit")

		case line == "prompt":
			edited, err := openEditor(currentPrompt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error opening editor: %v\n", err)
				continue
			}
			currentPrompt = edited

		default:
			if strings.HasPrefix(line, "prompt ") {
				currentPrompt = strings.TrimPrefix(line, "prompt ")
			} else if line == "ask" {
				if currentPrompt == "" {
					fmt.Println("No prompt set. Use 'prompt' to set one.")
					continue
				}
				if debugMode {
					fmt.Fprintf(os.Stderr, "[DEBUG] Asking prompt:\n%s\n", currentPrompt)
				}
				if pendingContext.Len() > 0 {
					currentPrompt += "\n\nAdditional Context:\n" + pendingContext.String()
					pendingContext.Reset()
				}

				maxChars := maxTokens * charsPerToken
				if len(currentPrompt) > maxChars {
					currentPrompt = currentPrompt[:maxChars]
				}

				ans, err := askChatGPT(currentPrompt)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					continue
				}
				currentAnswer = ans
				if originalPrompt == "" {
					originalPrompt = currentPrompt
				}
				sessionPath, _ := storeSession(currentPrompt, currentAnswer, originalPrompt)
				currentSessionPath = sessionPath
				fmt.Println("Answer:\n", currentAnswer)
				fmt.Fprintf(os.Stderr, "Session stored at: %s\n", sessionPath)

				// Extract all commands from currentAnswer
				currentCommands = extractCommands(currentAnswer)
				if len(currentCommands) > 1 {
					fmt.Printf("%d commands found. Type 'run' to list them or 'run N' to run a specific one.\n", len(currentCommands))
				} else if len(currentCommands) == 1 {
					fmt.Println("1 command found. Type 'run' to see it or 'run 1' to run it.")
				} else {
					fmt.Println("No commands found in the answer.")
				}

			} else if line == "refine" {
				// Refine logic
				if currentAnswer == "" {
					fmt.Println("No answer to refine. Use 'ask' first.")
					continue
				}

				// Load previous session outputs if available
				var runOutput, contextOutput string
				if currentSessionPath != "" {
					runOutput = readFileIfExists(filepath.Join(currentSessionPath, "run_output.txt"))
					contextOutput = readFileIfExists(filepath.Join(currentSessionPath, "context.txt"))
					orig, err := ioutil.ReadFile(filepath.Join(currentSessionPath, "original_prompt.txt"))
					if err == nil && len(orig) > 0 {
						originalPrompt = string(orig)
					}
				}

				initialRefine := "Refine this answer with the following context:\n\n---\nCurrent Answer:\n" + currentAnswer
				if runOutput != "" {
					initialRefine += "\n\nRun Output:\n" + runOutput
				}
				if contextOutput != "" {
					initialRefine += "\n\nAdditional Context:\n" + contextOutput
				}
				if originalPrompt != "" {
					initialRefine += "\n\nOriginal Prompt:\n" + originalPrompt
				}

				refineEditor, err := openEditor(initialRefine)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					continue
				}
				finalPrompt := "Refine this answer with the following context:\nORIGINAL PROMPT:\n" + originalPrompt +
					"\nANSWER:\n" + currentAnswer
				if runOutput != "" {
					finalPrompt += "\nRUN OUTPUT:\n" + runOutput
				}
				if contextOutput != "" {
					finalPrompt += "\nADDITIONAL CONTEXT:\n" + contextOutput
				}
				finalPrompt += "\nREFINEMENT:\n" + refineEditor
				if debugMode {
					fmt.Fprintf(os.Stderr, "[DEBUG] Refinement \n%s\n", refineEditor)
				}

				if debugMode {
					fmt.Fprintf(os.Stderr, "[DEBUG] Refine finalPrompt:\n%s\n", finalPrompt)
				}

				maxChars := maxTokens * charsPerToken
				if len(finalPrompt) > maxChars {
					finalPrompt = finalPrompt[:maxChars]
				}

				ans, err := askChatGPT(finalPrompt)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					continue
				}
				currentAnswer = ans
				sessionPath, _ := storeSession(finalPrompt, currentAnswer, originalPrompt)
				currentSessionPath = sessionPath
				fmt.Println("Refined Answer:\n", currentAnswer)
				fmt.Fprintf(os.Stderr, "Refined session stored at: %s\n", sessionPath)

				// Extract commands again after refinement if needed
				currentCommands = extractCommands(currentAnswer)

			} else if strings.HasPrefix(line, "run") {
				parts := strings.Split(line, " ")
				if len(parts) == 1 {
					// run with no arguments: list commands
					if len(currentCommands) == 0 {
						fmt.Println("No commands available.")
					} else {
						fmt.Println("Available commands:")
						for i, cmd := range currentCommands {
							fmt.Printf("%d: %s\n", i+1, cmd)
						}
					}
				} else {
					// run N
					if len(currentCommands) == 0 {
						fmt.Println("No commands available to run.")
						continue
					}
					nStr := parts[1]
					n, err := strconv.Atoi(nStr)
					if err != nil || n < 1 || n > len(currentCommands) {
						fmt.Println("Invalid command number.")
						continue
					}
					cmdStr := currentCommands[n-1]
					if currentSessionPath == "" {
						sessionPath, _ := storeSession(currentPrompt, currentAnswer, originalPrompt)
						currentSessionPath = sessionPath
					}
					if err := runCommandInteractively(cmdStr, currentSessionPath); err != nil {
						fmt.Fprintf(os.Stderr, "Error running command: %v\n", err)
					}
				}

			} else if line == "context" {
				fmt.Println("Enter a command to run for additional context:")
				ctxLine, err := rl.Readline()
				if err != nil && err == io.EOF {
					break
				}
				ctxLine = strings.TrimSpace(ctxLine)
				if ctxLine == "" {
					continue
				}
				addContextInInteractive(ctxLine, currentSessionPath, &pendingContext)

			} else if strings.HasPrefix(line, "context ") {
				cmdStr := strings.TrimPrefix(line, "context ")
				addContextInInteractive(cmdStr, currentSessionPath, &pendingContext)
			} else if line == "show" {
				fmt.Println("Current Prompt:\n", currentPrompt)
				fmt.Println("Current Answer:\n", currentAnswer)
			} else if line != "" {
				fmt.Println("Unknown command. Type 'help' for usage.")
			}
		}
	}
}
func extractCommands(answer string) []string {
	var commands []string
	lines := strings.Split(answer, "\n")

	inBlock := false
	var blockLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inBlock {
				// End of code block: parse block lines for commands
				for _, bl := range blockLines {
					if cmd := parseCommandLine(bl, inBlock); cmd != "" {
						commands = append(commands, cmd)
					}
				}
				blockLines = nil
				inBlock = false
			} else {
				// Start code block
				inBlock = true
				blockLines = nil
			}
		} else if inBlock {
			// Inside code block, collect lines
			blockLines = append(blockLines, line)
		} else {
			// Outside code block, check for $ lines
			if cmd := parseCommandLine(trimmed, inBlock); cmd != "" {
				commands = append(commands, cmd)
			}
		}
	}

	return commands
}

func parseCommandLine(line string, inBlock bool) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	// If inside a code block
	if inBlock {
		// Ignore comment lines in code block
		if strings.HasPrefix(trimmed, "#") {
			return ""
		}
		// Any non-empty, non-comment line inside a code block is a command
		return trimmed
	} else {
		// Outside code block, only lines starting with $ are commands
		if strings.HasPrefix(trimmed, "$ ") {
			return strings.TrimPrefix(trimmed, "$ ")
		}
		return ""
	}
}

func handleContext(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "No command provided for context.")
		os.Exit(1)
	}

	cmdStr := strings.Join(args, " ")

	_, _, sessionPath, err := getLastSession()
	if err != nil {
		// No session yet, store in pending context file
		output, cmdErr := runShellCommand(cmdStr)
		if cmdErr != nil {
			fmt.Fprintf(os.Stderr, "Error adding context: %v\n", cmdErr)
			fmt.Fprintln(os.Stderr, output)
			os.Exit(1)
		}
		appendToPendingContext(cmdStr, output)
		fmt.Println("Context added for future use (pending):", cmdStr)
		return
	}

	if err := addContextCommand(cmdStr, sessionPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error adding context: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Context added from command:", cmdStr)
}

func handleModels() {
	client := openai.NewClient(apiKey)
	ctx := context.Background()

	resp, err := client.ListModels(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing models: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Available Models:")
	for _, m := range resp.Models {
		if m.ID == model {
			fmt.Println("*", m.ID)
		} else {
			fmt.Println(m.ID)
		}
	}
}

func askChatGPT(prompt string) (string, error) {
	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Sending prompt to ChatGPT using model '%s' (max_tokens=%d):\n%s\n", model, maxTokens, prompt)
	}
	client := openai.NewClient(apiKey)
	ctx := context.Background()

	systemMessage := "You are a helpful assistant. The user might ask about commands or actions as if you could run them, but you cannot. " +
		"Do not refuse by stating inability to execute commands. Instead, provide instructions, examples, or guidance as if the user will run them themselves."

	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemMessage},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
	})
	if err != nil {
		return "", err
	}

	if len(resp.Choices) == 0 {
		return "", errors.New("no response from model")
	}

	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

func openEditor(initialContent string) (string, error) {
	if editor == "" {
		editor = "vi"
	}
	tmpfile, err := ioutil.TempFile("", "ask_prompt_*.md")
	if err != nil {
		return "", err
	}
	defer tmpfile.Close()

	if initialContent != "" {
		tmpfile.WriteString(initialContent)
		tmpfile.Sync()
	}

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Opening editor: %s %s\n", editor, tmpfile.Name())
	}

	cmd := exec.Command(editor, tmpfile.Name())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		return "", err
	}

	data, err := ioutil.ReadFile(tmpfile.Name())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func storeSession(prompt, answer, originalPrompt string) (string, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	sessionDir := filepath.Join(homedir, historyDirName)
	err = os.MkdirAll(sessionDir, 0755)
	if err != nil {
		return "", err
	}

	timestamp := time.Now().Format("20060102-150405")
	currentSessionPath := filepath.Join(sessionDir, timestamp)
	err = os.Mkdir(currentSessionPath, 0755)
	if err != nil {
		return "", err
	}

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Storing session in: %s\n", currentSessionPath)
	}

	err = ioutil.WriteFile(filepath.Join(currentSessionPath, "prompt.txt"), []byte(prompt), 0644)
	if err != nil {
		return "", err
	}

	err = ioutil.WriteFile(filepath.Join(currentSessionPath, "response.txt"), []byte(answer), 0644)
	if err != nil {
		return "", err
	}

	err = ioutil.WriteFile(filepath.Join(currentSessionPath, "original_prompt.txt"), []byte(originalPrompt), 0644)
	if err != nil {
		return "", err
	}

	return currentSessionPath, nil
}

func getLastSession() (string, string, string, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", err
	}

	sessionDir := filepath.Join(homedir, historyDirName)
	files, err := ioutil.ReadDir(sessionDir)
	if err != nil || len(files) == 0 {
		return "", "", "", errors.New("no previous sessions found")
	}

	latest := files[len(files)-1]
	sessionPath := filepath.Join(sessionDir, latest.Name())

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Last session path: %s\n", sessionPath)
	}

	promptData, err := ioutil.ReadFile(filepath.Join(sessionPath, "prompt.txt"))
	if err != nil {
		return "", "", "", err
	}
	responseData, err := ioutil.ReadFile(filepath.Join(sessionPath, "response.txt"))
	if err != nil {
		return "", "", "", err
	}

	return string(promptData), string(responseData), sessionPath, nil
}

func extractCommand(answer string) string {
	lines := strings.Split(answer, "\n")

	codeBlock := extractCodeBlock(lines)
	if codeBlock != "" {
		return strings.TrimSpace(codeBlock)
	}

	var commands []string
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "$ ") {
			commands = append(commands, strings.TrimPrefix(trim, "$ "))
		}
	}

	if len(commands) > 0 {
		return commands[0]
	}

	return ""
}

func extractCodeBlock(lines []string) string {
	inBlock := false
	var blockContent strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inBlock {
				return blockContent.String()
			}
			inBlock = true
			continue
		}
		if inBlock {
			blockContent.WriteString(line + "\n")
		}
	}
	return ""
}

func runCommandInteractively(cmdStr, sessionPath string) error {
	fmt.Printf("About to run: %s\nPress Enter to confirm or type 'edit' to modify. Ctrl+C to cancel.\n", cmdStr)
	var input string
	fmt.Scanln(&input)
	if input == "edit" {
		edited, err := openEditor(cmdStr)
		if err != nil {
			return fmt.Errorf("error opening editor: %w", err)
		}
		cmdStr = strings.TrimSpace(edited)
	}

	if cmdStr == "" {
		fmt.Println("No command to run after editing.")
		return nil
	}

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Running shell command: sh -c \"%s\"\n", cmdStr)
	}

	output, err := runShellCommand(cmdStr)

	if output != "" {
		ioutil.WriteFile(filepath.Join(sessionPath, "run_output.txt"), []byte(output), 0644)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, output)
		return fmt.Errorf("command failed: %w", err)
	}

	if output != "" {
		fmt.Println(output)
	}

	return nil
}

func addContextCommand(cmdStr, sessionPath string) error {
	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Running context command: sh -c \"%s\"\n", cmdStr)
	}
	output, err := runShellCommand(cmdStr)

	contextPath := filepath.Join(sessionPath, "context.txt")
	f, ferr := os.OpenFile(contextPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if ferr != nil {
		return ferr
	}
	defer f.Close()

	f.WriteString("\n---\nCommand: " + cmdStr + "\n" + output + "\n")

	if err != nil {
		fmt.Fprintln(os.Stderr, output)
		return fmt.Errorf("command failed: %w", err)
	}

	fmt.Println(output)
	return nil
}

func addContextInInteractive(cmdStr string, currentSessionPath string, pendingContext *strings.Builder) {
	output, err := runShellCommand(cmdStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error adding context: %v\n", err)
		fmt.Fprintln(os.Stderr, output)
		return
	}
	fmt.Println(output)

	if currentSessionPath != "" {
		contextPath := filepath.Join(currentSessionPath, "context.txt")
		f, ferr := os.OpenFile(contextPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "Error writing context: %v\n", ferr)
			return
		}
		defer f.Close()
		f.WriteString("\n---\nCommand: " + cmdStr + "\n" + output + "\n")
	} else {
		pendingContext.WriteString("\n---\nCommand: " + cmdStr + "\n" + output + "\n")
	}
}

func loadPendingContext() string {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(homedir, pendingContextFile)
	data, err := ioutil.ReadFile(path)
	if err == nil && len(data) > 0 {
		return string(data)
	}
	return ""
}

func clearPendingContext() {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(homedir, pendingContextFile)
	os.Remove(path)
}

func appendToPendingContext(cmdStr, output string) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(homedir, pendingContextFile)
	f, ferr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if ferr != nil {
		return
	}
	defer f.Close()
	f.WriteString("\n---\nCommand: " + cmdStr + "\n" + output + "\n")
}

func runShellCommand(cmdStr string) (string, error) {
	cmd := exec.Command("sh", "-c", cmdStr)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	output := outBuf.String() + errBuf.String()
	return output, err
}

func readFileIfExists(path string) string {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
