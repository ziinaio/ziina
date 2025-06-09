package ziina

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/giancarlosio/gorainbow"
	"github.com/gliderlabs/ssh"
	"github.com/urfave/cli/v2"
	sshcrypto "golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

const banner = `
███████╗██╗██╗███╗   ██╗ █████╗ 
╚══███╔╝██║██║████╗  ██║██╔══██╗
  ███╔╝ ██║██║██╔██╗ ██║███████║
 ███╔╝  ██║██║██║╚██╗██║██╔══██║
███████╗██║██║██║ ╚████║██║  ██║
╚══════╝╚═╝╚═╝╚═╝  ╚═══╝╚═╝  ╚═╝
`

var (
	// userSessions stores the individual SSH sessions.
	userSessions = make(map[string][]ssh.Session)

	// sessionName contains the name of the Zellij session.
	// An empty string denotes that the host has not yet initiaed a session.
	sessionName = ""

	// mu holds the mutex.
	mu sync.Mutex
)

// charset contains the list of available characters for random session-name generation.
const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomString returns a random string of characters of the given length.
func randomString(length int) (string, error) {
	result := make([]byte, length)
	for i := range result {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		result[i] = charset[num.Int64()]
	}
	return string(result), nil
}

// App serves as entry-point for github.com/urfave/cli
var App = &cli.App{
	Name:  "ziina",
	Usage: "💻 📤 👥 Instant terminal sharing; using Zellij." + "\n" + gorainbow.Rainbow(banner),
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "listen",
			Aliases: []string{"l"},
			Usage:   "Listen on this port.",
			Value:   "127.0.0.1:2222",
		},
		&cli.StringFlag{
			Name:    "server",
			Aliases: []string{"s"},
			Usage:   "The SSH server to use as endpoint.",
			Value:   "",
		},
		&cli.StringFlag{
			Name:    "user",
			Aliases: []string{"u"},
			Usage:   "Username for SSH authentication.",
		},
		&cli.StringFlag{
			Name:    "host-key",
			Aliases: []string{"k"},
			Usage:   "Path to the private key for SSH authentication.",
			Value:   "ssh_host_rsa_key",
		},
	},
	Action: func(ctx *cli.Context) error {
		// Separate out the port from the listen-address.
		parts := strings.Split(ctx.String("listen"), ":")
		if len(parts) != 2 {
			return fmt.Errorf("invalid listen address: %s", ctx.String("listen"))
		}

		listenHost := parts[0]
		if ctx.String("server") == "" && listenHost == "127.0.0.1" {
			return fmt.Errorf("address for remote ssh server not provided consider adding one with -s <server-addr> or make it accessible on your local network with -l 0.0.0.0:2222")
		}
		portStr := parts[1]
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return err
		}

		// Determine username for SSH authentication.
		username := ctx.String("user")
		if username == "" {
			// Use current user if none specified.
			currentUser, err := user.Current()
			if err != nil {
				return err
			}
			username = currentUser.Username
		}

		// Generate a random Zellij session-name.
		sessionName, err := randomString(7)
		if err != nil {
			return err
		}

		chGuard := make(chan struct{}, 2)

		// Start the remote port-forwarding tunnel if a server endpoint is specified.
		server := ctx.String("server")
		// Track which host to connect to for Zellij (server or local listener)
		serverOrHost := server
		if server != "" {
			go func() {
				if err := runReverseTunnel(chGuard, listenHost, server, username, port); err != nil {
					log.Fatalf("SSH remote port-forwarding tunnel terminated: %s\n", err)
				}
			}()
			<-chGuard
		} else {
			// Pure local mode; skip remote port forwarding
			log.Println("Skipping remote port-forwarding (local-only mode)")
		}

		// Start the SSH server
		go func() {
			if err := runServer(chGuard, ctx.String("listen"), sessionName, ctx.String("host-key")); err != nil {
				log.Fatalf("SSH server error: %v", err)
			}
		}()
		<-chGuard

		// Print connection info
		fmt.Println("")
		if server != "" {
			fmt.Printf("\tJoin via: ssh -p %d %s@%s\n", port, sessionName, server)
		}
		if listenHost != "127.0.0.1" {
			displayHost := listenHost
			if displayHost == "0.0.0.0" {
				displayHost = "<local-addr>"
			}
			fmt.Printf("\tDirect: ssh -p %d %s@%s\n", port, sessionName, displayHost)
		}
		fmt.Println("\nPress Enter to continue...")
		bufio.NewReader(os.Stdin).ReadBytes('\n')

		// Start the Zellij session over SSH
		if server == "" {
			serverOrHost = listenHost
		}
		return runZellij(serverOrHost, sessionName, port)
	},
}

func runServer(chGuard chan struct{}, listenAddr string, sessionName string, hostKeyFile string) error {
	// Define the SSH server
	server := &ssh.Server{
		Addr: listenAddr,
		Handler: func(s ssh.Session) {
			username := s.User()

			mu.Lock()
			// Disallow clients connecting with the wrong username.
			if sessionName == "" {
				sessionName = username
			}
			if username != sessionName {
				return
			}

			// Add session to the user pool
			userSessions[username] = append(userSessions[username], s)
			mu.Unlock()

			// The Zellij command.
			cmd := exec.Command("zellij", "-l", "compact", "attach", "--create", sessionName)

			// Zellij requires a PTY.
			ptyReq, winCh, isPty := s.Pty()
			if !isPty {
				io.WriteString(s, "No PTY requested. Zellij requires a PTY.\n")
				s.Exit(1)
				return
			}

			// Set TERM environment variable
			cmd.Env = append(cmd.Env, fmt.Sprintf("TERM=%s", ptyReq.Term))
			cmd.Env = append(cmd.Env, fmt.Sprintf("SHELL=%s", os.Getenv("SHELL")))

			// Start Zellij in a new PTY
			ptmx, err := pty.Start(cmd)
			if err != nil {
				log.Printf("Failed to start PTY: %v", err)
				s.Exit(1)
				return
			}
			defer ptmx.Close()

			// Handle window resize
			go func() {
				for win := range winCh {
					pty.Setsize(ptmx, &pty.Winsize{
						Cols: uint16(win.Width),
						Rows: uint16(win.Height),
					})
				}
			}()

			// Connect session input/output to the PTY
			go io.Copy(ptmx, s)
			io.Copy(s, ptmx) // blocks until Zellij exits
		},
	}

	// Load the host key from a file using golang.org/x/crypto/ssh to parse
	privateKeyPath := hostKeyFile
	keyBytes, err := os.ReadFile(privateKeyPath)
	if err == nil {
		private, err := sshcrypto.ParsePrivateKey(keyBytes)
		if err == nil {
			server.AddHostKey(private)
		}
	}

	go func() {
		chGuard <- struct{}{}
	}()

	log.Printf("Starting Ziina server on %s...\n", listenAddr)
	return server.ListenAndServe()
}

func runReverseTunnel(chGuard chan struct{}, bindAddr, remoteHost, user string, port int) error {
	log.Println("Starting SSH reverse port-forwarding...")

	// Connect to the running SSH agent
	sshAgentSocket := os.Getenv("SSH_AUTH_SOCK")
	if sshAgentSocket == "" {
		log.Fatalf("SSH agent not found. Please ensure SSH agent is running and SSH_AUTH_SOCK is set.")
	}

	// Open the agent socket
	agentConn, err := net.Dial("unix", sshAgentSocket)
	if err != nil {
		log.Fatalf("Failed to connect to SSH agent: %s", err)
	}
	defer agentConn.Close()

	// Create a new agent client
	agentClient := sshagent.NewClient(agentConn)

	// SSH client configuration
	config := &sshcrypto.ClientConfig{
		User: user, // Replace with your SSH username
		Auth: []sshcrypto.AuthMethod{
			// Use the SSH agent to retrieve keys for authentication
			sshcrypto.PublicKeysCallback(agentClient.Signers),
		},
		HostKeyCallback: sshcrypto.InsecureIgnoreHostKey(), // For development, replace with proper verification in production
	}

	client, err := sshcrypto.Dial("tcp", fmt.Sprintf("%s:22", remoteHost), config)
	if err != nil {
		return fmt.Errorf("failed to dial SSH server: %v", err)
	}

	// Request remote port forwarding
	listener, err := client.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return fmt.Errorf("failed to set up remote port forwarding: %v", err)
	}

	log.Printf("Remote port forwarding established: %s:%d -> localhost:%d", remoteHost, port, port)

	// Handle incoming connections
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Listener accept error: %v", err)
				continue
			}

			// Connect to the local SSH server
			localConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", bindAddr, port))
			if err != nil {
				log.Printf("Failed to connect to local service: %v", err)
				conn.Close()
				continue
			}

			// Start bidirectional copy
			go func() {
				defer conn.Close()
				defer localConn.Close()
				go io.Copy(localConn, conn)
				io.Copy(conn, localConn)
			}()
		}
	}()

	go func() {
		chGuard <- struct{}{}
	}()

	// Wait for interrupt signal to gracefully shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Println("Shutting down...")

	return nil
}

func runZellij(server, sessionName string, port int) error {
	// Connect to SSH agent
	sshAgentSock := os.Getenv("SSH_AUTH_SOCK")
	if sshAgentSock == "" {
		return fmt.Errorf("SSH_AUTH_SOCK not set")
	}
	agentConn, err := net.Dial("unix", sshAgentSock)
	if err != nil {
		return fmt.Errorf("failed to connect to SSH agent: %w", err)
	}
	defer agentConn.Close()
	ag := sshagent.NewClient(agentConn)

	// SSH config
	config := &sshcrypto.ClientConfig{
		User: sessionName,
		Auth: []sshcrypto.AuthMethod{
			sshcrypto.PublicKeysCallback(ag.Signers),
		},
		HostKeyCallback: sshcrypto.InsecureIgnoreHostKey(), // Don't use this in production
	}

	// Connect
	addr := fmt.Sprintf("%s:%d", server, port)
	client, err := sshcrypto.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer client.Close()

	// Create session
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	// Save current terminal state
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("failed to set terminal raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	// Handle Ctrl+C gracefully
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		term.Restore(fd, oldState)
		os.Exit(0)
	}()

	// Request PTY
	termType := os.Getenv("TERM")
	if termType == "" {
		termType = "xterm-256color"
	}
	width, height, err := term.GetSize(fd)
	if err != nil {
		width, height = 80, 24 // fallback
	}
	err = session.RequestPty(termType, height, width, sshcrypto.TerminalModes{
		sshcrypto.ECHO: 1,
	})
	if err != nil {
		return fmt.Errorf("request for PTY failed: %w", err)
	}

	// Set I/O
	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	// Start Zellij
	if err := session.Start("zellij attach " + sessionName); err != nil {
		return fmt.Errorf("failed to start zellij: %w", err)
	}

	// Wait for session to end
	if err := session.Wait(); err != nil {
		return fmt.Errorf("zellij session ended with error: %w", err)
	}

	return nil
}
