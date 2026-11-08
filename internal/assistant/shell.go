package assistant

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func classifyBash(command string) Decision {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) == 0 {
		return DecisionUnknown
	}
	worst := DecisionRead
	// The parser normalizes CR and CRLF, but Bash treats CR as literal data.
	if strings.ContainsRune(command, '\r') {
		worst = DecisionUnknown
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		effect := DecisionRead
		switch node := node.(type) {
		case nil:
			return false
		case *syntax.File, *syntax.BinaryCmd, *syntax.Block, *syntax.Subshell,
			*syntax.Lit, *syntax.SglQuoted, *syntax.DblQuoted:
		case *syntax.Comment:
			// Unlike Bash, the parser consumes a comment's backslash-newline as a continuation.
			if strings.HasSuffix(node.Text, "\\\n") {
				effect = DecisionUnknown
			}
		case *syntax.Stmt:
			if node.Coprocess || node.Disown {
				effect = DecisionUnknown
			}
		case *syntax.CallExpr:
			effect = classifyCall(node)
		case *syntax.Word:
			if _, ok := staticWord(node); !ok {
				effect = DecisionUnknown
			}
		case *syntax.Redirect:
			effect = classifyRedirect(node)
		default:
			effect = DecisionUnknown
		}
		worst = combineEffects(worst, effect)
		// Even unsupported syntax can contain executable substitutions or deletions.
		return true
	})
	return worst
}

func combineEffects(a, b Decision) Decision {
	for _, d := range []Decision{DecisionDelete, DecisionUnknown, DecisionWrite} {
		if a == d || b == d {
			return d
		}
	}
	return DecisionRead
}

func classifyCall(call *syntax.CallExpr) Decision {
	if len(call.Args) < 2 {
		return DecisionUnknown
	}
	fields := make([]string, len(call.Args))
	effect := DecisionRead
	for i, word := range call.Args {
		value, ok := staticWord(word)
		if !ok {
			effect = DecisionUnknown
		}
		fields[i] = value
	}
	binary := fields[0]
	if i := strings.LastIndexByte(binary, '/'); i >= 0 {
		binary = binary[i+1:]
	}
	if binary != "kontora" {
		return DecisionUnknown
	}
	switch verb := fields[1]; {
	case verb == "delete":
		return DecisionDelete
	case effect == DecisionUnknown:
		return DecisionUnknown
	case len(fields) == 3 && (fields[2] == "-h" || fields[2] == "--help"):
		// note, summary and new treat flags after a positional as text, not help.
		return DecisionRead
	case verb == "config" && slices.Contains(fields[2:], "edit"):
		return DecisionWrite
	case kontoraReadVerbs[verb]:
		return DecisionRead
	case kontoraWriteVerbs[verb]:
		return DecisionWrite
	default:
		return DecisionUnknown
	}
}

func classifyRedirect(redir *syntax.Redirect) Decision {
	if redir.N != nil && strings.Trim(redir.N.Value, "0123456789") != "" {
		return DecisionUnknown
	}
	switch redir.Op {
	case syntax.RdrIn, syntax.DplIn:
		return DecisionRead
	case syntax.RdrOut, syntax.AppOut, syntax.RdrInOut, syntax.DplOut, syntax.RdrClob,
		syntax.RdrAll, syntax.AppAll:
		return DecisionWrite
	case syntax.AppClob, syntax.RdrAllClob, syntax.AppAllClob, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		return DecisionUnknown
	default:
		return DecisionUnknown
	}
}

// staticWord decodes literal quoting without expanding words or executing code.
func staticWord(word *syntax.Word) (string, bool) {
	var out strings.Builder
	if !literalParts(&out, word.Parts, false) {
		return "", false
	}
	return out.String(), true
}

func literalParts(out *strings.Builder, parts []syntax.WordPart, quoted bool) bool {
	for _, part := range parts {
		switch part := part.(type) {
		case *syntax.Lit:
			for i := 0; i < len(part.Value); i++ {
				c := part.Value[i]
				if c == '\\' && i+1 < len(part.Value) {
					next := part.Value[i+1]
					if !quoted || strings.ContainsRune("$`\"\\\n", rune(next)) {
						i++
						if next != '\n' {
							out.WriteByte(next)
						}
						continue
					}
				}
				if !quoted && strings.ContainsRune("*?[]{}~", rune(c)) {
					return false
				}
				out.WriteByte(c)
			}
		case *syntax.SglQuoted:
			if part.Dollar {
				return false
			}
			out.WriteString(part.Value)
		case *syntax.DblQuoted:
			if part.Dollar || !literalParts(out, part.Parts, true) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
