# devopsy completion for fish. Load it at the first Tab after devopsy in each
# shell, from the installed devopsy, with a one-line completion file:
#
#   echo 'command -q devopsy; and devopsy --completion fish | source' > ~/.config/fish/completions/devopsy.fish
#
# It asks devopsy itself (devopsy --complete <words>) for targets, commands,
# flags and, through docker compose, services.

function __devopsy_complete
    set -l words (commandline -opc)[2..-1] (commandline -ct)
    set -l out (devopsy --complete $words 2>/dev/null)
    or return
    set -q out[1]; or return
    set -l directive (string replace ':' '' -- $out[-1])
    set -e out[-1]
    if set -q out[1]
        # Candidates end in their group: label project commands and docker
        # compose's, which otherwise look alike.
        for line in $out
            set -l f (string split \t -- $line)
            set -l label
            switch "$f[3]"
                case project
                    set label project
                case compose
                    set label 'docker compose'
                case '*'
                    string join \t -- $f[1..2]
                    continue
            end
            printf '%s\t%s%s\n' $f[1] $label (string replace -r '^(.)' ': $1' -- "$f[2]")
        end
    else if test (math "bitand($directive, 4)") -eq 0
        __fish_complete_path (commandline -ct)
    end
end

# Replaces earlier rules, so sourcing it again is clean. -k keeps devopsy's
# order: targets, project commands, then devopsy's and docker compose's.
complete -c devopsy -e
complete -c devopsy -f -k -a '(__devopsy_complete)'
