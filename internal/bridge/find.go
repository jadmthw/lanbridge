package bridge

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Instance is a Minecraft game folder (the official launcher's .minecraft or
// a launcher profile such as Prism, Modrinth or CurseForge).
type Instance struct {
	Dir       string `json:"dir"`
	Name      string `json:"name"`
	HasCarpet bool   `json:"hasCarpet"`
	Installed bool   `json:"installed"`
}

// Instances lists the Minecraft folders found on this computer plus extra ones.
func Instances(extra []string) []Instance {
	seen := map[string]bool{}
	var out []Instance
	for _, d := range append(candidateDirs(), extra...) {
		d = filepath.Clean(d)
		if seen[d] || !isGameDir(d) {
			continue
		}
		seen[d] = true
		out = append(out, Instance{Dir: d, Name: instanceName(d), HasCarpet: hasCarpet(d), Installed: fileExists(ScriptPath(d))})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].HasCarpet && !out[j].HasCarpet })
	return out
}

// ScriptPath is where the app goes so /script load finds it in every world.
func ScriptPath(gameDir string) string {
	return filepath.Join(gameDir, "config", "carpet", "scripts", AppName+".sc")
}

// Install copies the Scarpet app into a game folder.
func Install(gameDir string) (string, error) {
	p := ScriptPath(gameDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, Script, 0o644)
}

// Active finds the data folder of a world that is open right now and running
// the app, preferring the one with the freshest heartbeat.
func Active(extra []string) (dir string, h Hello, ok bool) {
	var best time.Time
	for _, inst := range Instances(extra) {
		worlds, _ := os.ReadDir(filepath.Join(inst.Dir, "saves"))
		for _, w := range worlds {
			if !w.IsDir() {
				continue
			}
			d := filepath.Join(inst.Dir, "saves", w.Name(), "scripts", AppName+".data")
			st, err := os.Stat(filepath.Join(d, "hello.json"))
			if err != nil || time.Since(st.ModTime()) > Stale || !st.ModTime().After(best) {
				continue
			}
			if hh, err := ReadHello(d); err == nil {
				dir, h, ok, best = d, hh, true, st.ModTime()
			}
		}
	}
	return
}

func candidateDirs() []string {
	home, _ := os.UserHomeDir()
	cfg, _ := os.UserConfigDir()
	var dirs, roots []string
	switch runtime.GOOS {
	case "windows":
		dirs = append(dirs, filepath.Join(os.Getenv("APPDATA"), ".minecraft"))
		roots = append(roots, filepath.Join(home, "curseforge", "minecraft", "Instances"), filepath.Join(os.Getenv("APPDATA"), "ATLauncher", "instances"))
	case "darwin":
		dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "minecraft"))
	default:
		dirs = append(dirs, filepath.Join(home, ".minecraft"), filepath.Join(home, ".var", "app", "com.mojang.Minecraft", ".minecraft"))
		roots = append(roots, filepath.Join(home, ".local", "share", "PrismLauncher", "instances"),
			filepath.Join(home, ".var", "app", "org.prismlauncher.PrismLauncher", "data", "PrismLauncher", "instances"),
			filepath.Join(home, ".local", "share", "ModrinthApp", "profiles"), filepath.Join(home, ".local", "share", "multimc", "instances"))
	}
	roots = append(roots, filepath.Join(cfg, "PrismLauncher", "instances"), filepath.Join(cfg, "ModrinthApp", "profiles"),
		filepath.Join(cfg, "com.modrinth.theseus", "profiles"), filepath.Join(cfg, "gdlauncher_carbon", "data", "instances"))
	for _, r := range roots {
		subs, _ := os.ReadDir(r)
		for _, s := range subs {
			if !s.IsDir() {
				continue
			}
			base := filepath.Join(r, s.Name())
			dirs = append(dirs, base, filepath.Join(base, ".minecraft"), filepath.Join(base, "minecraft"), filepath.Join(base, "instance"))
		}
	}
	return dirs
}

func isGameDir(d string) bool {
	return fileExists(filepath.Join(d, "saves")) || fileExists(filepath.Join(d, "options.txt"))
}

func hasCarpet(d string) bool {
	mods, _ := os.ReadDir(filepath.Join(d, "mods"))
	for _, m := range mods {
		n := strings.ToLower(m.Name())
		if strings.Contains(n, "carpet") && strings.HasSuffix(n, ".jar") && !strings.Contains(n, "extra") {
			return true
		}
	}
	return false
}

func instanceName(d string) string {
	home, _ := os.UserHomeDir()
	base, parent := filepath.Base(d), filepath.Dir(d)
	switch {
	case base == ".minecraft" && (parent == home || strings.EqualFold(parent, filepath.Clean(os.Getenv("APPDATA")))),
		base == "minecraft" && filepath.Base(parent) == "Application Support":
		return "Minecraft Launcher"
	case base == ".minecraft" || base == "minecraft" || base == "instance":
		return filepath.Base(parent) // launcher profile folder, e.g. Prism's instance name
	}
	return base
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
