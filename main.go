package main

import (
	"bytes"
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
	apiKey    = os.Getenv("OPENAI_API_KEY")
	editor    = os.Getenv("EDITOR")
	model     = "gpt-4"
	debugMode bool
)

func main() {
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Error: OPENAI_API_KEY is not set.")
		os.Exit(1)
	}

	refineCmd := flag.NewFlagSet("refine", flag.ExitOnError)
	interactiveCmd := flag.NewFlagSet("interactive", flag.ExitOnError)
	contextCmd := flag.NewFlagSet("context", flag.ExitOnError)

	// Global flags
	var fileFlag string
	var runFlag bool
	var debugFlag bool

	// We'll parse flags differently depending on subcommands
	if len(os.Args) < 2 {
		// no subcommand
		flag.StringVar(&fileFlag, "f", "", "file path containing prompt")
		flag.BoolVar(&runFlag, "run", false, "immediately run the resulting command if feasible")
		flag.BoolVar(&debugFlag, "debug", false, "enable debug output")
		flag.Parse()
		debugMode = debugFlag
		handleAsk(fileFlag, runFlag)
		return
	}

	switch os.Args[1] {
	case "refine":
		refineCmd.BoolVar(&debugFlag, "debug", false, "enable debug output")
		refineCmd.Parse(os.Args[2:])
		debugMode = debugFlag
		handleRefine(refineCmd.Args())
	case "interactive":
		interactiveCmd.BoolVar(&debugFlag, "debug", false, "enable debug output")
		interactiveCmd.Parse(os.Args[2:])
		debugMode = debugFlag
		handleInteractive(interactiveCmd.Args())
	case "context":
		contextCmd.BoolVar(&debugFlag, "debug", false, "enable debug output")
		contextCmd.Parse(os.Args[2:])
		debugMode = debugFlag
		handleContext(contextCmd.Args())
	default:
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		flag.CommandLine.StringVar(&fileFlag, "f", "", "file path containing prompt")
		flag.CommandLine.BoolVar(&runFlag, "run", false, "immediately run the resulting command if feasible")
		flag.CommandLine.BoolVar(&debugFlag, "debug", false, "enable debug output")
		flag.CommandLine.Parse(os.Args[1:])
		debugMode = debugFlag

		args := flag.CommandLine.Args()
		var prompt string
		if len(args) > 0 {
			prompt = strings.Join(args, " ")
		}
		doAsk(prompt, fileFlag, runFlag)
	}
}

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
		prompt = strings.Join(flag.Args(), " ")
	} else {
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

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Asking prompt:\n%s\n", prompt)
	}

	answer, err := askChatGPT(prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting response: %v\n", err)
		os.Exit(1)
	}

	sessionPath, err := storeSession(prompt, answer, prompt) // original prompt = prompt
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

func handleRefine(args []string) {
	lastPrompt, lastResponse, lastSessionPath, err := getLastSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error retrieving last session: %v\n", err)
		os.Exit(1)
	}

	originalPrompt, err := ioutil.ReadFile(filepath.Join(lastSessionPath, "original_prompt.txt"))
	if err != nil {
		// If not found, fallback to lastPrompt as original (shouldn't happen if properly stored)
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
	fmt.Println("Entering interactive mode. Type 'help' for commands, 'exit' to quit.")
	var currentPrompt string
	var currentAnswer string
	var currentSessionPath string
	var originalPrompt string

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
			fmt.Println("Commands:\n- prompt: edit the current prompt\n- ask: submit the current prompt to ChatGPT\n- refine: refine the current answer\n- run: attempt to extract and run a command from the current answer\n- context: run a shell command to add context\n- show: display current prompt/answer\n- exit: quit")
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
			if debugMode {
				fmt.Fprintf(os.Stderr, "[DEBUG] Asking prompt:\n%s\n", currentPrompt)
			}
			ans, err := askChatGPT(currentPrompt)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				continue
			}
			currentAnswer = ans
			// First ask in interactive sets originalPrompt
			if originalPrompt == "" {
				originalPrompt = currentPrompt
			}
			sessionPath, _ := storeSession(currentPrompt, currentAnswer, originalPrompt)
			currentSessionPath = sessionPath
			fmt.Println("Answer:\n", currentAnswer)
			fmt.Fprintf(os.Stderr, "Session stored at: %s\n", sessionPath)
		case "refine":
			if currentAnswer == "" {
				fmt.Println("No answer to refine. Use 'ask' first.")
				continue
			}
			var runOutput, contextOutput string
			if currentSessionPath != "" {
				runOutput = readFileIfExists(filepath.Join(currentSessionPath, "run_output.txt"))
				contextOutput = readFileIfExists(filepath.Join(currentSessionPath, "context.txt"))
				orig, err := ioutil.ReadFile(filepath.Join(currentSessionPath, "original_prompt.txt"))
				if err == nil && len(orig) > 0 {
					originalPrompt = string(orig)
				}
			}

			initialRefine := "Add your refinement:\n\n---\nCurrent Answer:\n" + currentAnswer
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
				fmt.Fprintf(os.Stderr, "[DEBUG] Refine finalPrompt:\n%s\n", finalPrompt)
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
			if currentSessionPath == "" {
				sessionPath, _ := storeSession(currentPrompt, currentAnswer, originalPrompt)
				currentSessionPath = sessionPath
			}
			if err := runCommandInteractively(cmdStr, currentSessionPath); err != nil {
				fmt.Fprintf(os.Stderr, "Error running command: %v\n", err)
			}
		case "context":
			fmt.Println("Enter a command to run for additional context:")
			var ctxCmdLine string
			_, err := fmt.Scanln(&ctxCmdLine)
			if err != nil && err == io.EOF {
				break
			}
			if currentSessionPath == "" {
				if currentAnswer == "" {
					fmt.Println("No session found. Use 'ask' first to create a session before adding context.")
					continue
				} else {
					// If there's an answer but no sessionPath, store it now
					sessionPath, _ := storeSession(currentPrompt, currentAnswer, originalPrompt)
					currentSessionPath = sessionPath
				}
			}
			if err := addContextCommand(ctxCmdLine, currentSessionPath); err != nil {
				fmt.Fprintf(os.Stderr, "Error adding context: %v\n", err)
			} else {
				fmt.Println("Context added from command:", ctxCmdLine)
			}
		case "show":
			fmt.Println("Current Prompt:\n", currentPrompt)
			fmt.Println("Current Answer:\n", currentAnswer)
		default:
			fmt.Println("Unknown command. Type 'help' for usage.")
		}
	}
}

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

	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Asking prompt:\n%s\n", prompt)
	}

	answer, err := askChatGPT(prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	sessionPath, err := storeSession(prompt, answer, prompt) // original prompt = prompt if first time
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not store session: %v\n", err)
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

func handleContext(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "No command provided for context.")
		os.Exit(1)
	}

	cmdStr := strings.Join(args, " ")

	_, _, sessionPath, err := getLastSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "No previous session found. Run `ask` first.\n")
		os.Exit(1)
	}

	if err := addContextCommand(cmdStr, sessionPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error adding context: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Context added from command:", cmdStr)
}

func askChatGPT(prompt string) (string, error) {
	if debugMode {
		fmt.Fprintf(os.Stderr, "[DEBUG] Sending prompt to ChatGPT:\n%s\n", prompt)
	}
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

	// Store or copy original prompt
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
	if err != nil {
		return "", "", "", err
	}
	if len(files) == 0 {
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

	cmd := exec.Command("sh", "-c", cmdStr)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdin = os.Stdin
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	output := outBuf.String() + errBuf.String()

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
	cmd := exec.Command("sh", "-c", cmdStr)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	output := outBuf.String() + errBuf.String()

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

func readFileIfExists(path string) string {
	data, err := ioutil.ReadFile(path)
	if err == nil {
		return string(data)
	}
	return ""
}
