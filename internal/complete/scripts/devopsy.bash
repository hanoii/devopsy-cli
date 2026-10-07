# devopsy completion for bash. Load it from ~/.bashrc:
#
#   source <(devopsy --completion bash)
#
# It asks devopsy itself (devopsy --complete <words>) for targets, commands,
# flags and, through docker compose, services.

_devopsy() {
    local IFS=$'\n' out directive line
    COMPREPLY=()
    out=$(devopsy --complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null) || return
    directive=${out##*:}
    out=${out%:*}
    for line in $out; do
        line=${line%%$'\t'*}
        [[ -n $line && $line == "${COMP_WORDS[COMP_CWORD]}"* ]] && COMPREPLY+=("$line")
    done
    # devopsy's order: targets, then project commands first (bash 4.4 or
    # later).
    compopt -o nosort 2>/dev/null
    if (( directive & 2 )); then
        compopt -o nospace 2>/dev/null
    fi
    if (( directive & 4 )); then
        compopt +o default 2>/dev/null
    fi
}

complete -o default -F _devopsy devopsy
