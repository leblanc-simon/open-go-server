package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// D138 : une application Java écoute sur un port de la plage 26xxx, que Spring
// Boot lit dans SERVER_PORT, et se lance par le lien géré « java -jar ».
func TestJavaÉcouteSurUnPortEtSeLanceParLeLienGéré(t *testing.T) {
	prov, proj := fixture(t, model.KindJava)
	proj.Spec.App.Runtime = "21.0.5-tem"
	if !Applies(proj) {
		t.Fatal("java doit relever du provider applicatif")
	}
	endpoint, err := prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Unix() || endpoint.Address != "127.0.0.1:26001" {
		t.Errorf("Java écoute sur un port de la plage 26xxx : %+v", endpoint)
	}
	content, err := prov.Build(proj, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	lien := filepath.Join(proj.Spec.Layout.Root, ".local", "bin", "java")
	for _, attendu := range []string{
		"ExecStart=" + lien + " $JAVA_OPTS -jar app.jar\n",
		"Environment=SERVER_PORT=26001\n", "Environment=SERVER_ADDRESS=127.0.0.1\n",
		"Environment=PORT=26001\n",
	} {
		if !strings.Contains(text, attendu) {
			t.Errorf("%q absent de l'unité :\n%s", attendu, text)
		}
	}
	if strings.Contains(text, ".sdkman") {
		t.Errorf("aucune version ne doit apparaître dans l'unité :\n%s", text)
	}

	proj.Spec.App.Entrypoint = "target/boutique.jar"
	content, err = prov.Build(proj, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), " -jar target/boutique.jar\n") {
		t.Errorf("l'entrypoint désigne le jar :\n%s", content)
	}
}

// D138 : le JDK vient de SDKMAN, dans le home, sous l'identité du projet, sans
// toucher ~/.bashrc ni poser de question.
func TestJavaSInstalleParSDKMANDansLeHome(t *testing.T) {
	rec := exec.NewRecorder()
	installer := &Installer{Exec: &realExecutor{rec}}
	proj := model.New("shop.fr", naming.EnvProd, model.KindJava)
	if err := proj.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if err := installer.Install(context.Background(), proj, RuntimeJava, "21.0.5-tem"); err != nil {
		t.Fatal(err)
	}
	calls := rec.Calls()
	if len(calls) != 1 || calls[0].AsUser != proj.Spec.User.Name || calls[0].Name != "/bin/bash" {
		t.Fatalf("installation sous l'identité du projet, par bash : %+v", calls)
	}
	script := calls[0].Args[1]
	for _, attendu := range []string{
		filepath.Join(proj.Spec.Layout.Root, ".sdkman"), "rcupdate=false", "ci=true", "sdk install java 21.0.5-tem",
	} {
		if !strings.Contains(script, attendu) {
			t.Errorf("%q absent du script : %s", attendu, script)
		}
	}
	if err := installer.Install(context.Background(), proj, RuntimeJava, "21; rm -rf /"); err == nil {
		t.Error("une version qui n'est pas un nom doit être refusée")
	}
	if got := BinaryPath("/home/shop.fr", RuntimeJava, "21.0.5-tem"); got !=
		"/home/shop.fr/.sdkman/candidates/java/21.0.5-tem/bin/java" {
		t.Errorf("binaire : %s", got)
	}
}

// La version se lit dans le premier mot numérique : « openjdk 21.0.5 … ».
func TestLaVersionSeLitDansLePremierMotNumérique(t *testing.T) {
	for sortie, attendu := range map[string]string{
		"openjdk 21.0.5 2024-10-15\nOpenJDK Runtime Environment\n": "21.0.5",
		"v22.11.0\n": "22.11.0",
		"ruby 3.3.6 (2024-11-05 revision 75015d4c1f) [x86_64-linux]\n": "3.3.6",
		"Python 3.12.7\n": "3.12.7",
	} {
		if got := parseVersion(sortie); got != attendu {
			t.Errorf("%q : %q, attendu %q", sortie, got, attendu)
		}
	}
}
