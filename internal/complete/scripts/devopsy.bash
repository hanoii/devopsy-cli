# devopsy completion for bash. Load it from ~/.bashrc:
#
#   source <(devopsy --completion bash)
#
# It asks devopsy itself (devopsy --complete <words>) for targets, commands,
# flags and, through docker compose, services.

_devopsy() {
    local IFS=$' \t\n' out directive line words args cur colon
    COMPREPLY=()
    # Words from the line itself: bash splits COMP_WORDS at ":", which
    # targets use (@vm1:prod).
    line=${COMP_LINE:0:COMP_POINT}
    read -r -a words <<< "$line"
    [[ $line == *[[:space:]] || ${#words[@]} -eq 1 ]] && words+=("")
    cur=${words[${#words[@]}-1]}
    # bash replaces only what follows the last ":" in the word.
    colon=
    [[ $cur == *:* && $COMP_WORDBREAKS == *:* ]] && colon=${cur%"${cur##*:}"}
    # Sliced before IFS changes: bash 3.2 (macOS) joins a quoted slice with
    # IFS.
    args=("${words[@]:1}")
    IFS=$'\n'
    out=$(devopsy --complete "${args[@]}" 2>/dev/null) || return
    directive=${out##*:}
    out=${out%:*}
    for line in $out; do
        line=${line%%$'\t'*}
        [[ -n $line && $line == "$cur"* ]] && COMPREPLY+=("${line#"$colon"}")
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
