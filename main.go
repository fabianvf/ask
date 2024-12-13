package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

const (
	historyDirName = ".ask/sessions"
)

var (
	apiKey = os.Getenv("OPENAI_API_KEY")
	editor = os.Getenv("EDITOR")
	model  = "gpt-4" // or "gpt-3.5-turbo", configurable
)

func main() {
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Error: OPENAI_API_KEY is not set.")
		os.Exit(1)
	}

	refineCmd := flag.NewFlagSet("refine", flag.ExitOnError)
	interactiveCmd := flag.NewFlagSet("interactive", flag.ExitOnError)

	fileFlag := flag.String("f", "", "file path containing prompt")
	runFlag := flag.Bool("run", false, "immediately run the resulting command if safe/feasible")

	// If no arguments, we default to the 'ask' behavior
	if len(os.Args) < 2 {
		flag.Parse()
		handleAsk(*fileFlag, *runFlag)
		return
	}

	switch os.Args[1] {
	case "refine":
		refineCmd.Parse(os.Args[2:])
		handleRefine(refineCmd.Args())
	case "interactive":
		interactiveCmd.Parse(os.Args[2:])
		handleInteractive(interactiveCmd.Args())
	default:
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		flag.CommandLine.StringVar(fileFlag, "f", "", "file path containing prompt")
		flag.CommandLine.BoolVar(runFlag, "run", false, "immediately run the resulting command if safe/feasible")
		flag.CommandLine.Parse(os.Args[1:])

		args := flag.CommandLine.Args()
		var prompt string
		if len(args) > 0 {
			prompt = strings.Join(args, " ")
		}
		doAsk(prompt, *fileFlag, *runFlag)
	}
}

// handleAsk is called when user runs `ask` without subcommands but with flags
func handleAsk(filePath string, run bool) {
	prompt := ""
	if filePath != "" {
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to read file: %v\n", err)
			os.Exit(1)
		}
		prompt = string(data)
	} else if len(flag.Args()) > 0 {
		// user typed: ask "prompt..."
		prompt = strings.Join(flag.Args(), " ")
	} else {
		// open editor
		edited, err := openEditor("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open editor: %v\n", err)
			os.Exit(1)
		}
		prompt = edited
	}

	if prompt == "" {
		fmt.Fprintln(os.Stderr, "No prompt provided.")
		os.Exit(1)
	}

	answer, err := askChatGPT(prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting response: %v\n", err)
		os.Exit(1)
	}

	sessionPath, err := storeSession(prompt, answer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Could not store session: %v\n", err)
	}

	fmt.Println(answer)

	if run {
		cmdStr := extractCommand(answer)
		if cmdStr != "" {
			runCommandInteractively(cmdStr)
		} else {
			fmt.Fprintln(os.Stderr, "No runnable command found in the answer.")
		}
	} else {
		fmt.Fprintf(os.Stderr, "Session stored in: %s\n", sessionPath)
	}
}

// handleRefine is called when user runs `ask refine`
func handleRefine(args []string) {
	// The idea: automatically use the last session for refinement
	lastPrompt, lastResponse, err := getLastSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error retrieving last session: %v\n", err)
		os.Exit(1)
	}

	refinement := ""
	if len(args) > 0 {
		// Use command-line arguments as the refinement context
		refinement = strings.Join(args, " ")
	} else {
		// open editor to get refinement context
		edited, err := openEditor("Provide refinement/context below:\n\n---\nPrevious Response:\n" + lastResponse)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open editor: %v\n", err)
			os.Exit(1)
		}
		refinement = edited
	}

	finalPrompt := "Refine the following response with the additional context:\n\nPREVIOUS PROMPT:\n" + lastPrompt + "\n\nPREVIOUS RESPONSE:\n" + lastResponse + "\n\nREFINEMENT CONTEXT:\n" + refinement

	answer, err := askChatGPT(finalPrompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting refinement: %v\n", err)
		os.Exit(1)
	}

	sessionPath, err := storeSession(finalPrompt, answer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Could not store session: %v\n", err)
	}

	fmt.Println(answer)
	fmt.Fprintf(os.Stderr, "Refined session stored in: %s\n", sessionPath)
}

// handleInteractive: Starts an interactive mode where you can load a previous session, refine, and run commands.
func handleInteractive(args []string) {
	fmt.Println("Entering interactive mode. Type 'help' for commands, 'exit' to quit.")
	var currentPrompt string
	var currentAnswer string
	for {
		fmt.Print("> ")
		var line string
		_, err := fmt.Scanln(&line)
		if err != nil && err == io.EOF {
			break
		}
		switch line {
		case "exit":
			return
		case "help":
			fmt.Println("Commands:\n- prompt: edit the current prompt\n- ask: submit the current prompt to ChatGPT\n- refine: refine the current answer\n- run: attempt to extract and run a command from the current answer\n- show: display current prompt/answer\n- exit: quit")
		case "prompt":
			edited, err := openEditor(currentPrompt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error opening editor: %v\n", err)
				continue
			}
			currentPrompt = edited
		case "ask":
			if currentPrompt == "" {
				fmt.Println("No prompt set. Use 'prompt' to set one.")
				continue
			}
			ans, err := askChatGPT(currentPrompt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				continue
			}
			currentAnswer = ans
			sessionPath, _ := storeSession(currentPrompt, currentAnswer)
			fmt.Println("Answer:\n", currentAnswer)
			fmt.Fprintf(os.Stderr, "Session stored at: %s\n", sessionPath)
		case "refine":
			if currentAnswer == "" {
				fmt.Println("No answer to refine. Use 'ask' first.")
				continue
			}
			refineEditor, err := openEditor("Add your refinement:")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				continue
			}
			finalPrompt := "Refine this answer with the following context:\nANSWER:\n" + currentAnswer + "\nREFINEMENT:\n" + refineEditor
			ans, err := askChatGPT(finalPrompt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				continue
			}
			currentAnswer = ans
			sessionPath, _ := storeSession(finalPrompt, currentAnswer)
			fmt.Println("Refined Answer:\n", currentAnswer)
			fmt.Fprintf(os.Stderr, "Refined session stored at: %s\n", sessionPath)
		case "run":
			if currentAnswer == "" {
				fmt.Println("No answer to run. Use 'ask' first.")
				continue
			}
			cmdStr := extractCommand(currentAnswer)
			if cmdStr == "" {
				fmt.Println("No command found in the current answer.")
				continue
			}
			runCommandInteractively(cmdStr)
		case "show":
			fmt.Println("Current Prompt:\n", currentPrompt)
			fmt.Println("Current Answer:\n", currentAnswer)
		default:
			fmt.Println("Unknown command. Type 'help' for usage.")
		}
	}
}

// doAsk performs the main ask logic when prompt or file flag are directly provided
func doAsk(prompt, filePath string, run bool) {
	if prompt == "" && filePath != "" {
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to read file: %v\n", err)
			os.Exit(1)
		}
		prompt = string(data)
	}

	if prompt == "" {
		edited, err := openEditor("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open editor: %v\n", err)
			os.Exit(1)
		}
		prompt = edited
	}

	answer, err := askChatGPT(prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	sessionPath, err := storeSession(prompt, answer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not store session: %v\n", err)
	}
	fmt.Println(answer)

	if run {
		cmdStr := extractCommand(answer)
		if cmdStr != "" {
			runCommandInteractively(cmdStr)
		} else {
			fmt.Fprintln(os.Stderr, "No runnable command found in the answer.")
		}
	} else {
		fmt.Fprintf(os.Stderr, "Session stored in: %s\n", sessionPath)
	}
}

func askChatGPT(prompt string) (string, error) {
	client := openai.NewClient(apiKey)
	ctx := context.Background()

	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
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
		editor = "vi" // fallback if EDITOR not set
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

func storeSession(prompt, answer string) (string, error) {
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

	err = ioutil.WriteFile(filepath.Join(currentSessionPath, "prompt.txt"), []byte(prompt), 0644)
	if err != nil {
		return "", err
	}

	err = ioutil.WriteFile(filepath.Join(currentSessionPath, "response.txt"), []byte(answer), 0644)
	if err != nil {
		return "", err
	}

	return currentSessionPath, nil
}

// getLastSession retrieves the last stored session from the history directory.
func getLastSession() (string, string, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}

	sessionDir := filepath.Join(homedir, historyDirName)
	files, err := ioutil.ReadDir(sessionDir)
	if err != nil {
		return "", "", err
	}
	if len(files) == 0 {
		return "", "", errors.New("no previous sessions found")
	}

	// Files should be in alphabetical order; if named by timestamp, last is latest
	latest := files[len(files)-1]
	sessionPath := filepath.Join(sessionDir, latest.Name())

	promptData, err := ioutil.ReadFile(filepath.Join(sessionPath, "prompt.txt"))
	if err != nil {
		return "", "", err
	}
	responseData, err := ioutil.ReadFile(filepath.Join(sessionPath, "response.txt"))
	if err != nil {
		return "", "", err
	}

	return string(promptData), string(responseData), nil
}

// readStdin reads all data from stdin if available
func readStdin() (string, error) {
	stat, _ := os.Stdin.Stat()
	if stat.Mode()&os.ModeCharDevice != 0 {
		// No data on stdin
		return "", errors.New("no stdin input")
	}
	data, err := io.ReadAll(os.Stdin)
	return string(data), err
}

// Improved command extraction logic
func extractCommand(answer string) string {
	lines := strings.Split(answer, "\n")

	// First, try to extract from code blocks
	codeBlock := extractCodeBlock(lines)
	if codeBlock != "" {
		// Return entire code block content as the command to run
		return strings.TrimSpace(codeBlock)
	}

	// If no code block command found, look for $-prefixed commands
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

// Extract code block content
func extractCodeBlock(lines []string) string {
	inBlock := false
	var blockContent strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inBlock {
				// End of code block
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

func runCommandInteractively(cmdStr string) {
	fmt.Printf("About to run: %s\nPress Enter to confirm or type 'edit' to modify. Ctrl+C to cancel.\n", cmdStr)
	var input string
	fmt.Scanln(&input)
	if input == "edit" {
		edited, err := openEditor(cmdStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening editor: %v\n", err)
			return
		}
		cmdStr = strings.TrimSpace(edited)
	}

	if cmdStr == "" {
		fmt.Println("No command to run after editing.")
		return
	}

	cmdParts := strings.Split(cmdStr, " ")
	execCmd := exec.Command(cmdParts[0], cmdParts[1:]...)
	execCmd.Stdin = os.Stdin
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr
	err := execCmd.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running command: %v\n", err)
	}
}
