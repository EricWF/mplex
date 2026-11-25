package completion

import (
	"fmt"
	"strings"
)

// SupportedShells lists the shells supported by Generate.
var SupportedShells = []string{"fish", "zsh", "bash"}

// Generate returns an idiomatic shell completion script for the requested shell
// ("fish", "zsh", or "bash").
func Generate(shell string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "fish":
		return fishScript, nil
	case "zsh":
		return zshScript, nil
	case "bash":
		return bashScript, nil
	default:
		return "", fmt.Errorf("unsupported shell %q (supported: %s)", shell, strings.Join(SupportedShells, ", "))
	}
}

const fishScript = `# Fish shell completions for mplex
# Install: mplex completion fish > ~/.config/fish/completions/mplex.fish

function __mplex_socket_args
    set -l cmd (commandline -opc)
    set -l i 2
    while test $i -le (count $cmd)
        switch $cmd[$i]
            case --socket
                if test (math $i + 1) -le (count $cmd)
                    echo --socket
                    echo $cmd[(math $i + 1)]
                end
                return
            case '--socket=*'
                echo $cmd[$i]
                return
        end
        set i (math $i + 1)
    end
end

function __mplex_sessions
    set -l sock_args (__mplex_socket_args)
    mplex $sock_args __complete-sessions --describe 2>/dev/null
end

function __mplex_needs_command
    set -l cmd (commandline -opc)
    set -l i 2
    while test $i -le (count $cmd)
        switch $cmd[$i]
            case --socket --key --scrollback --replay-lines --naming
                set i (math $i + 2)
                continue
            case '-*'
                set i (math $i + 1)
                continue
            case '*'
                return 1
        end
    end
    return 0
end

function __mplex_using_command
    set -l cmd (commandline -opc)
    set -l i 2
    while test $i -le (count $cmd)
        switch $cmd[$i]
            case --socket --key --scrollback --replay-lines --naming
                set i (math $i + 2)
                continue
            case '-*'
                set i (math $i + 1)
                continue
            case '*'
                contains -- $cmd[$i] $argv
                return $status
        end
    end
    return 1
end

complete -c mplex -f

# Global flags
complete -c mplex -n __mplex_needs_command -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n __mplex_needs_command -l key -r -d 'Switcher trigger key'
complete -c mplex -n __mplex_needs_command -l scrollback -r -d 'Max scrollback lines per session'
complete -c mplex -n __mplex_needs_command -l replay-lines -r -d 'Max scrollback lines replayed on switch'
complete -c mplex -n __mplex_needs_command -l window-title -d 'Tag outer terminal window title with [session]'
complete -c mplex -n __mplex_needs_command -l naming -r -a 'cwd numeric' -d 'Default session naming mode'
complete -c mplex -n __mplex_needs_command -l pick -d 'Open session switcher immediately'
complete -c mplex -n __mplex_needs_command -l nested -d 'Allow nesting mplex inside an existing session'
complete -c mplex -n __mplex_needs_command -l version -d 'Print version and exit'
complete -c mplex -n __mplex_needs_command -l help -d 'Show help information'

# Top-level subcommands
complete -c mplex -n __mplex_needs_command -a new -d 'Create a new session'
complete -c mplex -n __mplex_needs_command -a attach -d 'Attach to an existing session'
complete -c mplex -n __mplex_needs_command -a switch -d 'Open switcher or switch active window'
complete -c mplex -n __mplex_needs_command -a list -d 'List active sessions'
complete -c mplex -n __mplex_needs_command -a capture -d 'Read session screen or scrollback'
complete -c mplex -n __mplex_needs_command -a read -d 'Read session screen or scrollback'
complete -c mplex -n __mplex_needs_command -a logs -d 'Read session screen or scrollback'
complete -c mplex -n __mplex_needs_command -a send -d 'Send input or keys to a session'
complete -c mplex -n __mplex_needs_command -a follow -d 'Stream live output from a session'
complete -c mplex -n __mplex_needs_command -a wait -d 'Wait for pattern or idle state in a session'
complete -c mplex -n __mplex_needs_command -a mcp -d 'Run MCP server over stdio'
complete -c mplex -n __mplex_needs_command -a rename -d 'Rename a session'
complete -c mplex -n __mplex_needs_command -a kill -d 'Kill one or all sessions'
complete -c mplex -n __mplex_needs_command -a detach -d 'Detach an active client'
complete -c mplex -n __mplex_needs_command -a save -d 'Save active sessions to a workspace snapshot'
complete -c mplex -n __mplex_needs_command -a restore -d 'Restore sessions from a workspace snapshot'
complete -c mplex -n __mplex_needs_command -a config -d 'Inspect or initialize configuration'
complete -c mplex -n __mplex_needs_command -a completion -d 'Generate shell completion script'
complete -c mplex -n __mplex_needs_command -a update -d 'Check for updates and upgrade in place'
complete -c mplex -n __mplex_needs_command -a version -d 'Print version and exit'
complete -c mplex -n __mplex_needs_command -a help -d 'Show help information'

# Top-level shorthand: mplex <session>
complete -c mplex -n __mplex_needs_command -a '(__mplex_sessions)'

# new
complete -c mplex -n '__mplex_using_command new n' -s d -d 'Create session detached'
complete -c mplex -n '__mplex_using_command new n' -s c -r -a '(__fish_complete_directories)' -d 'Starting working directory'
complete -c mplex -n '__mplex_using_command new n' -l json -d 'Create detached and output JSON'
complete -c mplex -n '__mplex_using_command new n' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command new n' -l key -r -d 'Switcher trigger key'

# attach
complete -c mplex -n '__mplex_using_command attach a' -l create -d 'Create session if missing'
complete -c mplex -n '__mplex_using_command attach a' -l pick -d 'Open session switcher'
complete -c mplex -n '__mplex_using_command attach a' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command attach a' -l key -r -d 'Switcher trigger key'
complete -c mplex -n '__mplex_using_command attach a' -a '(__mplex_sessions)'

# switch
complete -c mplex -n '__mplex_using_command switch s pick' -l client -r -d 'Target client ID'
complete -c mplex -n '__mplex_using_command switch s pick' -l json -d 'Output result as JSON'
complete -c mplex -n '__mplex_using_command switch s pick' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command switch s pick' -a '(__mplex_sessions)'

# list
complete -c mplex -n '__mplex_using_command list ls' -l json -d 'Output sessions as JSON'
complete -c mplex -n '__mplex_using_command list ls' -l socket -r -F -d 'Daemon Unix socket path'

# capture / read / logs
complete -c mplex -n '__mplex_using_command capture read logs' -s f -l follow -d 'Stream live output continuously (read-only)'
complete -c mplex -n '__mplex_using_command capture read logs' -l lines -r -d 'Limit output to last N lines'
complete -c mplex -n '__mplex_using_command capture read logs' -l scrollback -d 'Include scrollback history'
complete -c mplex -n '__mplex_using_command capture read logs' -l ansi -d 'Preserve ANSI escape sequences'
complete -c mplex -n '__mplex_using_command capture read logs' -l raw-wrap -d 'Keep raw terminal row breaks'
complete -c mplex -n '__mplex_using_command capture read logs' -l json -d 'Output captured text as JSON'
complete -c mplex -n '__mplex_using_command capture read logs' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command capture read logs' -a '(__mplex_sessions)'

# send
complete -c mplex -n '__mplex_using_command send write' -l enter -d 'Append Enter after input'
complete -c mplex -n '__mplex_using_command send write' -l key -r -a 'enter tab esc space bs up down left right home end pgup pgdn del ctrl-c ctrl-d ctrl-z ctrl-l' -d 'Special key to send'
complete -c mplex -n '__mplex_using_command send write' -l stdin -d 'Read raw input from stdin'
complete -c mplex -n '__mplex_using_command send write' -l json -d 'Output result as JSON'
complete -c mplex -n '__mplex_using_command send write' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command send write' -a '(__mplex_sessions)'

# follow
complete -c mplex -n '__mplex_using_command follow' -l history -d 'Emit existing screen/scrollback before live bytes'
complete -c mplex -n '__mplex_using_command follow' -l lines -r -d 'Initial history lines to emit with --history'
complete -c mplex -n '__mplex_using_command follow' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command follow' -a '(__mplex_sessions)'

# wait
complete -c mplex -n '__mplex_using_command wait' -l match -r -d 'Regex pattern to match in session output'
complete -c mplex -n '__mplex_using_command wait' -l idle -r -d 'Wait until session output is quiet for duration (e.g. 2s)'
complete -c mplex -n '__mplex_using_command wait' -l exit -d 'Wait until session exits'
complete -c mplex -n '__mplex_using_command wait' -l scrollback -d 'Search scrollback as well as visible screen'
complete -c mplex -n '__mplex_using_command wait' -l timeout -r -d 'Maximum time to wait (default 30s)'
complete -c mplex -n '__mplex_using_command wait' -l json -d 'Output wait result as JSON'
complete -c mplex -n '__mplex_using_command wait' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command wait' -a '(__mplex_sessions)'

# mcp
complete -c mplex -n '__mplex_using_command mcp' -l socket -r -F -d 'Daemon Unix socket path'

# rename
complete -c mplex -n '__mplex_using_command rename' -l json -d 'Output renamed session as JSON'
complete -c mplex -n '__mplex_using_command rename' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command rename' -a '(__mplex_sessions)'

# kill
complete -c mplex -n '__mplex_using_command kill rm' -l all -d 'Kill all sessions'
complete -c mplex -n '__mplex_using_command kill rm' -l json -d 'Output killed sessions as JSON'
complete -c mplex -n '__mplex_using_command kill rm' -l socket -r -F -d 'Daemon Unix socket path'
complete -c mplex -n '__mplex_using_command kill rm' -a '(__mplex_sessions)'

# detach
complete -c mplex -n '__mplex_using_command detach d' -l client -r -d 'Client ID to detach'
complete -c mplex -n '__mplex_using_command detach d' -l json -d 'Output result as JSON'
complete -c mplex -n '__mplex_using_command detach d' -l socket -r -F -d 'Daemon Unix socket path'

# save
complete -c mplex -n '__mplex_using_command save' -l file -r -F -d 'Explicit snapshot file path'
complete -c mplex -n '__mplex_using_command save' -l with-scrollback -d 'Include scrollback tail lines in snapshot'
complete -c mplex -n '__mplex_using_command save' -l lines -r -d 'Scrollback tail lines per session'
complete -c mplex -n '__mplex_using_command save' -l json -d 'Output saved snapshot as JSON'
complete -c mplex -n '__mplex_using_command save' -l socket -r -F -d 'Daemon Unix socket path'

# restore
complete -c mplex -n '__mplex_using_command restore' -l file -r -F -d 'Explicit snapshot file path'
complete -c mplex -n '__mplex_using_command restore' -l force -d 'Create deduplicated sessions even if names exist'
complete -c mplex -n '__mplex_using_command restore' -l json -d 'Output restore summary as JSON'
complete -c mplex -n '__mplex_using_command restore' -l socket -r -F -d 'Daemon Unix socket path'

# config
complete -c mplex -n '__mplex_using_command config' -l path -d 'Print resolved config file path'
complete -c mplex -n '__mplex_using_command config' -l init -d 'Write a starter config.toml'
complete -c mplex -n '__mplex_using_command config' -l force -d 'Overwrite existing config when used with --init'
complete -c mplex -n '__mplex_using_command config' -l json -d 'Output effective config as JSON'
complete -c mplex -n '__mplex_using_command config' -l socket -r -F -d 'Daemon Unix socket path'

# completion
complete -c mplex -n '__mplex_using_command completion' -a 'fish zsh bash' -d 'Shell type'

# update
complete -c mplex -n '__mplex_using_command update upgrade' -l check -d 'Check for updates without installing'
complete -c mplex -n '__mplex_using_command update upgrade' -s y -l yes -d 'Upgrade without prompting'
complete -c mplex -n '__mplex_using_command update upgrade' -l force -d 'Install latest release even if up to date'
complete -c mplex -n '__mplex_using_command update upgrade' -l json -d 'Output result as JSON'
complete -c mplex -n '__mplex_using_command update upgrade' -l socket -r -F -d 'Daemon Unix socket path'
`

const zshScript = `#compdef mplex
# Zsh shell completions for mplex
# Install: mplex completion zsh > "${fpath[1]}/_mplex"
# Or add to ~/.zshrc: eval "$(mplex completion zsh)"

_mplex_socket_args() {
    local i
    for (( i = 2; i < ${#words[@]}; i++ )); do
        case "${words[i]}" in
            --socket)
                if (( i + 1 <= ${#words[@]} )); then
                    reply=(--socket "${words[i+1]}")
                    return
                fi
                ;;
            --socket=*)
                reply=("${words[i]}")
                return
                ;;
        esac
    done
    reply=()
}

_mplex_sessions() {
    local -a reply sessions
    local line name desc escaped
    _mplex_socket_args
    while IFS=$'\t' read -r name desc; do
        [[ -z "$name" ]] && continue
        escaped="${name//:/\\:}"
        if [[ -n "$desc" ]]; then
            sessions+=("${escaped}:${desc}")
        else
            sessions+=("${escaped}")
        fi
    done < <(mplex "${reply[@]}" __complete-sessions --describe 2>/dev/null)
    _describe -t sessions 'mplex session' sessions
}

_mplex() {
    local context state state_descr line
    typeset -A opt_args

    local -a commands
    commands=(
        'new:Create a new session'
        'attach:Attach to an existing session'
        'switch:Open switcher or switch active window'
        'list:List active sessions'
        'capture:Read session screen or scrollback'
        'read:Read session screen or scrollback'
        'logs:Read session screen or scrollback'
        'send:Send input or keys to a session'
        'follow:Stream live output from a session'
        'wait:Wait for pattern or idle state in a session'
        'mcp:Run MCP server over stdio'
        'rename:Rename a session'
        'kill:Kill one or all sessions'
        'detach:Detach an active client'
        'save:Save active sessions to a workspace snapshot'
        'restore:Restore sessions from a workspace snapshot'
        'config:Inspect or initialize configuration'
        'completion:Generate shell completion script'
        'update:Check for updates and upgrade in place'
        'version:Print version and exit'
        'help:Show help information'
    )

    _arguments -C \
        '--socket=[Daemon Unix socket path]:socket file:_files' \
        '--key=[Switcher trigger key]:key spec:' \
        '--scrollback=[Max scrollback lines per session]:lines:' \
        '--replay-lines=[Max scrollback lines replayed on switch]:lines:' \
        '--window-title[Tag outer terminal window title with session name]' \
        '--naming=[Default session naming mode]:mode:(cwd numeric)' \
        '--pick[Open session switcher immediately]' \
        '--nested[Allow nesting mplex inside an existing session]' \
        '--version[Print version and exit]' \
        '(-h --help)'{-h,--help}'[Show help information]' \
        '1:command:->command' \
        '*::arg:->args'

    case "$state" in
        command)
            _describe -t commands 'mplex command' commands
            _mplex_sessions
            ;;
        args)
            case "${words[1]}" in
                new|n)
                    _arguments \
                        '-d[Create session detached]' \
                        '-c[Starting working directory]:directory:_directories' \
                        '--json[Create detached and output JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '--key=[Switcher trigger key]:key spec:' \
                        '1:session name:' \
                        '*::command:_command_names -e'
                    ;;
                attach|a)
                    _arguments \
                        '--create[Create session if missing]' \
                        '--pick[Open session switcher]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '--key=[Switcher trigger key]:key spec:' \
                        '1:session:_mplex_sessions'
                    ;;
                switch|s|pick)
                    _arguments \
                        '--client=[Target client ID]:client id:' \
                        '--json[Output result as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:session:_mplex_sessions'
                    ;;
                list|ls)
                    _arguments \
                        '--json[Output sessions as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files'
                    ;;
                capture|read|logs)
                    _arguments \
                        '(-f --follow)'{-f,--follow}'[Stream live output continuously (read-only)]' \
                        '--lines=[Limit output to last N lines]:lines:' \
                        '--scrollback[Include scrollback history]' \
                        '--ansi[Preserve ANSI escape sequences]' \
                        '--raw-wrap[Keep raw terminal row breaks]' \
                        '--json[Output captured text as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:session:_mplex_sessions'
                    ;;
                send|write)
                    _arguments \
                        '--enter[Append Enter after input]' \
                        '*--key=[Special key to send]:key:(enter tab esc space bs up down left right home end pgup pgdn del ctrl-c ctrl-d ctrl-z ctrl-l)' \
                        '--stdin[Read raw input from stdin]' \
                        '--json[Output result as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:session:_mplex_sessions' \
                        '*:text:'
                    ;;
                follow)
                    _arguments \
                        '--history[Emit existing screen/scrollback before live bytes]' \
                        '--lines=[Initial history lines to emit with --history]:lines:' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:session:_mplex_sessions'
                    ;;
                wait)
                    _arguments \
                        '--match=[Regex pattern to match in session output]:regex:' \
                        '--idle=[Wait until session output is quiet for duration]:duration:' \
                        '--exit[Wait until session exits]' \
                        '--scrollback[Search scrollback as well as screen]' \
                        '--timeout=[Maximum time to wait]:duration:' \
                        '--json[Output result as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:session:_mplex_sessions'
                    ;;
                mcp)
                    _arguments \
                        '--socket=[Daemon Unix socket path]:socket file:_files'
                    ;;
                rename)
                    _arguments \
                        '--json[Output renamed session as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:session:_mplex_sessions' \
                        '2:new name:'
                    ;;
                kill|rm)
                    _arguments \
                        '--all[Kill all sessions]' \
                        '--json[Output killed sessions as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:session:_mplex_sessions'
                    ;;
                detach|d)
                    _arguments \
                        '--client=[Client ID to detach]:client id:' \
                        '--json[Output result as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files'
                    ;;
                save)
                    _arguments \
                        '--file=[Explicit snapshot file path]:snapshot file:_files' \
                        '--with-scrollback[Include scrollback tail lines in snapshot]' \
                        '--lines=[Scrollback tail lines per session]:lines:' \
                        '--json[Output saved snapshot as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:workspace name:'
                    ;;
                restore)
                    _arguments \
                        '--file=[Explicit snapshot file path]:snapshot file:_files' \
                        '--force[Create deduplicated sessions even if names exist]' \
                        '--json[Output restore summary as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files' \
                        '1:workspace name:'
                    ;;
                config)
                    _arguments \
                        '--path[Print resolved config file path]' \
                        '--init[Write a starter config.toml]' \
                        '--force[Overwrite existing config when used with --init]' \
                        '--json[Output effective config as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files'
                    ;;
                completion)
                    _arguments '1:shell:(fish zsh bash)'
                    ;;
                update|upgrade)
                    _arguments \
                        '--check[Check for updates without installing]' \
                        '(-y --yes)'{-y,--yes}'[Upgrade without prompting]' \
                        '--force[Install latest release even if up to date]' \
                        '--json[Output result as JSON]' \
                        '--socket=[Daemon Unix socket path]:socket file:_files'
                    ;;
            esac
            ;;
    esac
}

if [ "$funcstack[1]" = "_mplex" ]; then
    _mplex "$@"
else
    compdef _mplex mplex
fi
`

const bashScript = `# Bash shell completions for mplex
# Install: mplex completion bash > ~/.local/share/bash-completion/completions/mplex
# Or add to ~/.bashrc: source <(mplex completion bash)

_mplex_completions() {
    local cur prev word subcmd=""
    local -a sock_args=()
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev=""
    if (( COMP_CWORD > 0 )); then
        prev="${COMP_WORDS[COMP_CWORD-1]}"
    fi

    local i=1
    while (( i < COMP_CWORD )); do
        word="${COMP_WORDS[i]}"
        case "$word" in
            --socket)
                if (( i + 1 < COMP_CWORD )); then
                    sock_args=(--socket "${COMP_WORDS[i+1]}")
                fi
                (( i += 2 ))
                continue
                ;;
            --socket=*)
                sock_args=("$word")
                (( i++ ))
                continue
                ;;
            --key|--scrollback|--replay-lines|--naming)
                (( i += 2 ))
                continue
                ;;
            -*)
                (( i++ ))
                continue
                ;;
            *)
                if [[ -z "$subcmd" ]]; then
                    subcmd="$word"
                fi
                (( i++ ))
                ;;
        esac
    done

    # Complete values for option flags that expect paths or specific keywords.
    case "$prev" in
        --socket|--file)
            COMPREPLY=( $(compgen -f -- "$cur") )
            return 0
            ;;
        -c)
            COMPREPLY=( $(compgen -d -- "$cur") )
            return 0
            ;;
        --naming)
            COMPREPLY=( $(compgen -W "cwd numeric" -- "$cur") )
            return 0
            ;;
        --key)
            if [[ "$subcmd" == "send" || "$subcmd" == "write" ]]; then
                COMPREPLY=( $(compgen -W "enter tab esc space bs up down left right home end pgup pgdn del ctrl-c ctrl-d ctrl-z ctrl-l" -- "$cur") )
            else
                COMPREPLY=( $(compgen -W "ctrl-\\ ctrl-space ctrl-] alt-s none" -- "$cur") )
            fi
            return 0
            ;;
    esac

    local global_flags="--socket --key --scrollback --replay-lines --window-title --naming --pick --nested --version --help"
    local commands="new attach switch list capture read logs send follow wait mcp rename kill detach save restore config completion update version help"

    local sessions=""
    _mplex_load_sessions() {
        sessions="$(mplex "${sock_args[@]}" __complete-sessions 2>/dev/null)"
    }

    if [[ -z "$subcmd" ]]; then
        if [[ "$cur" == -* ]]; then
            COMPREPLY=( $(compgen -W "$global_flags" -- "$cur") )
            return 0
        fi
        _mplex_load_sessions
        local -a cmd_matches=() sess_matches=()
        cmd_matches=( $(compgen -W "$commands" -- "$cur") )
        if [[ -n "$sessions" ]]; then
            local IFS=$'\n'
            sess_matches=( $(compgen -W "$sessions" -- "$cur") )
        fi
        COMPREPLY=( "${cmd_matches[@]}" "${sess_matches[@]}" )
        return 0
    fi

    local flags=""
    local complete_sessions=0
    case "$subcmd" in
        new|n)
            flags="-d -c --json --socket --key"
            ;;
        attach|a)
            flags="--create --pick --socket --key"
            complete_sessions=1
            ;;
        switch|s|pick)
            flags="--client --json --socket"
            complete_sessions=1
            ;;
        list|ls)
            flags="--json --socket"
            ;;
        capture|read|logs)
            flags="-f --follow --lines --scrollback --ansi --raw-wrap --json --socket"
            complete_sessions=1
            ;;
        send|write)
            flags="--enter --key --stdin --json --socket"
            complete_sessions=1
            ;;
        follow)
            flags="--history --lines --socket"
            complete_sessions=1
            ;;
        wait)
            flags="--match --idle --exit --scrollback --timeout --json --socket"
            complete_sessions=1
            ;;
        mcp)
            flags="--socket"
            ;;
        rename)
            flags="--json --socket"
            complete_sessions=1
            ;;
        kill|rm)
            flags="--all --json --socket"
            complete_sessions=1
            ;;
        detach|d)
            flags="--client --json --socket"
            ;;
        save)
            flags="--file --with-scrollback --lines --json --socket"
            ;;
        restore)
            flags="--file --force --json --socket"
            ;;
        config)
            flags="--path --init --force --json --socket"
            ;;
        completion)
            COMPREPLY=( $(compgen -W "fish zsh bash" -- "$cur") )
            return 0
            ;;
        update|upgrade)
            flags="--check -y --yes --force --json --socket"
            ;;
        version|help)
            return 0
            ;;
    esac

    if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
        return 0
    fi

    if (( complete_sessions == 1 )); then
        _mplex_load_sessions
        if [[ -n "$sessions" ]]; then
            local IFS=$'\n'
            COMPREPLY=( $(compgen -W "$sessions" -- "$cur") )
        fi
    fi
    return 0
}

complete -F _mplex_completions mplex
`
