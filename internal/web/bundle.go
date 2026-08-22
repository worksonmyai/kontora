package web

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// uiEntry is the module esbuild starts from, relative to the source root.
const uiEntry = "index.js"

// uiNamespace keeps the UI sources out of esbuild's own file namespace: the
// plugin reads them through an fs.FS (the embed, or the dev directory), never
// off disk by absolute path.
const uiNamespace = "kontora-ui"

// webDirEnv holds a repo root to serve the UI from instead of the embedded
// copy. serveAsset reads it per request, so an edit shows up on a reload.
const webDirEnv = "KONTORA_WEB_DIR"

func devWebDir() string { return os.Getenv(webDirEnv) }

// uiBundle returns the compiled web UI as one script.
//
// The dev path reads $DIR/internal/web/ui and nothing else: falling back to
// the embedded copy per file would resurrect a module deleted from the working
// tree. Its error names the variable and the directory, because every way of
// pointing it somewhere wrong ends in the same esbuild message about a missing
// entry point.
func uiBundle() ([]byte, error) {
	dir := devWebDir()
	if dir == "" {
		return embeddedUIBundle()
	}
	src := filepath.Join(dir, "internal", "web", "ui")
	bundle, err := buildUIBundle(os.DirFS(src))
	if err != nil {
		return nil, fmt.Errorf("%s=%s: building the web UI from %s: %w", webDirEnv, dir, src, err)
	}
	return bundle, nil
}

// embeddedUIBundle compiles the sources baked into the binary. They cannot
// change while the process runs, so the result is built once and kept.
var embeddedUIBundle = sync.OnceValues(func() ([]byte, error) {
	sources, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return nil, err
	}
	return buildUIBundle(sources)
})

// buildUIBundle compiles the UI sources in sources into a single script.
// Errors carry esbuild's own messages, which name the file and line.
//
// A warning fails the build too. esbuild reports a duplicate object key as a
// warning, and that is the defect a component split across a dozen mixin
// literals invites: the later definition silently wins and the method it
// replaced is simply gone.
func buildUIBundle(sources fs.FS) ([]byte, error) {
	result := api.Build(api.BuildOptions{
		EntryPoints: []string{uiEntry},
		Bundle:      true,
		// index.html loads /app.js as a plain non-deferred tag while Alpine is
		// defer, so the bundle has to run first. A module is implicitly
		// deferred and would run after Alpine started.
		Format: api.FormatIIFE,
		Target: api.ES2022,
		// A browser stack trace otherwise points into a generated file that
		// exists nowhere on disk. Inline keeps it same-origin, so the CSP in
		// origin.go needs nothing added.
		Sourcemap: api.SourceMapInline,
		Plugins:   []api.Plugin{uiPlugin(sources)},
		Write:     false,
		LogLevel:  api.LogLevelSilent,
	})
	if msgs := buildMessages(result); msgs != "" {
		return nil, errors.New(msgs)
	}
	if len(result.OutputFiles) != 1 {
		return nil, fmt.Errorf("web UI bundle: esbuild produced %d output files, want 1", len(result.OutputFiles))
	}
	return result.OutputFiles[0].Contents, nil
}

// buildMessages formats everything esbuild complained about, or returns "" when
// it complained about nothing.
func buildMessages(result api.BuildResult) string {
	var out []string
	out = append(out, api.FormatMessages(result.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})...)
	out = append(out, api.FormatMessages(result.Warnings, api.FormatMessagesOptions{Kind: api.WarningMessage})...)
	return strings.TrimSpace(strings.Join(out, ""))
}

// uiPlugin teaches esbuild to read from sources instead of the filesystem.
func uiPlugin(sources fs.FS) api.Plugin {
	return api.Plugin{
		Name: "kontora-ui",
		Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{Filter: `.*`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				if args.Kind == api.ResolveEntryPoint {
					// Not args.Path: esbuild rewrites the entry to
					// "./index.js" when the daemon's working directory holds a
					// file of that name, and fs.FS rejects that spelling.
					return api.OnResolveResult{Path: uiEntry, Namespace: uiNamespace}, nil
				}
				if strings.HasPrefix(args.Path, "/") {
					return externalVendorImport(args)
				}
				// There is no package manager here, so a bare import would
				// resolve to nothing. Fail the build instead.
				if !strings.HasPrefix(args.Path, "./") && !strings.HasPrefix(args.Path, "../") {
					return api.OnResolveResult{}, fmt.Errorf(
						"%q is imported by %q but is not a relative path", args.Path, args.Importer)
				}
				resolved, err := resolveUISource(sources, path.Join(path.Dir(args.Importer), args.Path))
				if err != nil {
					return api.OnResolveResult{}, err
				}
				return api.OnResolveResult{Path: resolved, Namespace: uiNamespace}, nil
			})

			build.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: uiNamespace},
				func(args api.OnLoadArgs) (api.OnLoadResult, error) {
					body, err := fs.ReadFile(sources, args.Path)
					if err != nil {
						return api.OnLoadResult{}, err
					}
					contents := string(body)
					return api.OnLoadResult{Contents: &contents, Loader: api.LoaderJS}, nil
				})
		},
	}
}

// externalVendorImport handles an absolute path: a URL the browser fetches
// itself, which is how the UI reaches the vendored xterm and yaml modules.
//
// Only a dynamic import() may go out this way. esbuild turns an external
// static import under FormatIIFE into a __require() call whose shim throws in
// the browser, which kills the bundle before it assigns globalThis.kontora and
// leaves a blank page behind a green build. The target has to exist too, so a
// version bump that misses one of these paths fails here rather than 404ing at
// runtime.
func externalVendorImport(args api.OnResolveArgs) (api.OnResolveResult, error) {
	if args.Kind != api.ResolveJSDynamicImport {
		return api.OnResolveResult{}, fmt.Errorf(
			"%q is imported by %q with a static import; a vendored module must be reached through import()",
			args.Path, args.Importer)
	}
	if _, err := fs.Stat(staticFS, "static"+args.Path); err != nil {
		return api.OnResolveResult{}, fmt.Errorf("no vendored asset at %q, imported by %q", args.Path, args.Importer)
	}
	return api.OnResolveResult{Path: args.Path, External: true}, nil
}

// resolveUISource turns an import target into a path that exists in sources,
// adding .js when the import carries no extension.
func resolveUISource(sources fs.FS, target string) (string, error) {
	if path.Ext(target) == "" {
		target += ".js"
	}
	if _, err := fs.Stat(sources, target); err != nil {
		return "", fmt.Errorf("no web UI module at %q", target)
	}
	return target, nil
}
