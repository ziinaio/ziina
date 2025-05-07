# Ziina

💻 📤 👥 Instant terminal sharing; using Zellij.

With Ziina you can invite peers into your local Zellij session over the internet.
The peers only need a standard SSH client.

Ziina configures a SSH remote port-forwarding tunnel on any server you like, pointing back to a local high-port.
It then starts a minimal SSH server on that port that throws connecting clients directly into a Zellij session.
Match-making is accomplished via the username the clients connect with.

## Installation

```
go install github.com/ziinaio/ziina@latest
```

## Usage

```
NAME:
   ziina - 💻 📤 👥 Instant terminal sharing; using Zellij.

           ███████╗██╗██╗███╗   ██╗ █████╗
           ╚══███╔╝██║██║████╗  ██║██╔══██╗
             ███╔╝ ██║██║██╔██╗ ██║███████║
            ███╔╝  ██║██║██║╚██╗██║██╔══██║
           ███████╗██║██║██║ ╚████║██║  ██║
           ╚══════╝╚═╝╚═╝╚═╝ ╚══╝╚═╝ ╚═╝


USAGE:
   ziina [global options] command [command options]

COMMANDS:
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --listen value, -l value  Listen on this port. (default: ":2222")
   --server value, -s value  The SSH server to use as endpoint.
   --user value, -u value    Username for SSH authentication.
   --key value, -k value     Path to the private key for SSH authentication. (default: "/home/baccenfutter/.ssh/id_rsa")
   --help, -h                show help
```

---

Made with :heart: at :artificial_satellite: c-base, Berlin.
