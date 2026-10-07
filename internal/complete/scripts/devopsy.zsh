#compdef devopsy
# devopsy completion for zsh. Load it from ~/.zshrc, after compinit:
#
#   source <(devopsy --completion zsh)
#
# or save it as _devopsy in a directory of $fpath. It asks devopsy itself
# (devopsy --complete <words>) for targets, commands, flags and, through
# docker compose, services. Targets, project commands, devopsy's own
# commands and docker compose's are listed in their own groups, in that
# order.

_devopsy() {
    local out directive line value rest desc group item ret=1
    local -a lines opts targets project builtin compose plain
    out=$(devopsy --complete "${(@)words[2,CURRENT]}" 2>/dev/null) || return 1
    lines=("${(@f)out}")
    directive=${lines[-1]#:}
    for line in "${(@)lines[1,-2]}"; do
        [[ -z $line ]] && continue
        value=${line%%$'\t'*}
        rest= desc= group=
        [[ $line == *$'\t'* ]] && rest=${line#*$'\t'}
        desc=${rest%%$'\t'*}
        [[ $rest == *$'\t'* ]] && group=${rest#*$'\t'}
        item="${value//:/\\:}${desc:+:$desc}"
        case $group in
            target) targets+=("$item") ;;
            project) project+=("$item") ;;
            devopsy) builtin+=("$item") ;;
            compose) compose+=("$item") ;;
            *) plain+=("$item") ;;
        esac
    done
    (( directive & 2 )) && opts=(-S '')
    # Show the groups, unless the user configured it otherwise.
    zstyle -m ":completion:${curcontext}:" group-name '*' ||
        zstyle ":completion:*:*:devopsy:*" group-name ''
    zstyle -m ":completion:${curcontext}:descriptions" format '*' ||
        zstyle ":completion:*:*:devopsy:*:descriptions" format '%B%d%b'
    (( ${#targets} )) && _describe -t devopsy-targets target targets $opts && ret=0
    (( ${#project} )) && _describe -t project-commands 'project command' project $opts && ret=0
    (( ${#builtin} )) && _describe -t devopsy-commands devopsy builtin $opts && ret=0
    (( ${#compose} )) && _describe -t compose 'docker compose' compose $opts && ret=0
    (( ${#plain} )) && _describe -t values value plain $opts && ret=0
    if (( ret && ! (directive & 4) )); then
        _files && ret=0
    fi
    return ret
}

if [[ $funcstack[1] == _devopsy ]]; then
    _devopsy "$@"
else
    compdef _devopsy devopsy
fi
