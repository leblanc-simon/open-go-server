package template

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Valeurs que l'analyseur d'ini de FPM lirait autrement qu'elles ne s'écrivent
// (D115, R4-29). Chacune doit parvenir telle quelle.
var valeursLitterales = []string{
	".:/a;/b", "${HOME}x", "a${HOME}", "$x", "l'a", "a & b", "a=b", "a/b~c", "(x)", "!x", "a|b", "a^b",
	"no", "Off", "null", "TRUE", `a"`, `a";b`, `"x" ${HOME}`, `C:\temp`, `x\$y`, "x$y",
	"{\"a\":1}", "256M", "/usr/sbin/sendmail -t -i", "ondemand",
}

func TestFPMValueCiteCeQueLAnalyseurLiraitAutrement(t *testing.T) {
	for _, v := range valeursLitterales {
		for _, key := range []string{"php_admin_value[t]", "env"} {
			rendered := FPMValue(key, v)
			if FPMQuotedForm(v) {
				continue
			}
			if rendered != v && !strings.HasPrefix(rendered, `"`) {
				t.Errorf("%s %q : rendu %s, ni nu ni cité", key, v, rendered)
			}
		}
	}
	// La citation de l'exploitant (D103), l'expression de constantes et le
	// booléen restent nus.
	for _, c := range [][3]string{
		{"php_admin_value[sendmail_path]", `"/usr/sbin/sendmail -t -i"`, `"/usr/sbin/sendmail -t -i"`},
		{"php_value[error_reporting]", "E_ALL & ~E_DEPRECATED", "E_ALL & ~E_DEPRECATED"},
		{"php_admin_flag[log_errors]", "on", "on"},
		{"catch_workers_output", "yes", "yes"},
		{"php_admin_value[memory_limit]", "256M", "256M"},
		{"env", "prod", "prod"},
		{"env", "E_ALL", `"E_ALL"`},
		{"php_value[x]", "on", `"on"`},
	} {
		if got := FPMValue(c[0], c[1]); got != c[2] {
			t.Errorf("FPMValue(%s, %q) = %s, attendu %s", c[0], c[1], got, c[2])
		}
	}
	if got := FPMRead(`"a\"b" 'c' "\$d\\"`); got != `a"bc$d\` {
		t.Errorf("FPMRead : %q", got)
	}
}

// Le même jugement, rendu par un vrai php-fpm (« -tt » affiche ce qu'il a lu).
// Le banc se saute de lui-même sans FPM sur la machine (§16.3).
func TestFPMLitLesValeursCiteesTellesQuelles(t *testing.T) {
	var bins []string
	for _, v := range []string{"7.4", "8.0", "8.1", "8.2", "8.3", "8.4", "8.5"} {
		if _, err := os.Stat("/usr/sbin/php-fpm" + v); err == nil {
			bins = append(bins, "/usr/sbin/php-fpm"+v)
		}
	}
	if len(bins) == 0 {
		t.Skip("php-fpm absent")
	}
	for _, bin := range bins {
		t.Run(filepath.Base(bin), func(t *testing.T) { fpmLitTellesQuelles(t, bin) })
	}
}

func fpmLitTellesQuelles(t *testing.T, bin string) {
	dir := t.TempDir()
	var conf strings.Builder
	fmt.Fprintf(&conf, "[global]\nerror_log = %s/err.log\n[t]\nlisten = 127.0.0.1:0\npm = ondemand\npm.max_children = 1\n", dir)
	for i, v := range valeursLitterales {
		fmt.Fprintf(&conf, "php_admin_value[t%d] = %s\n", i, FPMValue(fmt.Sprintf("php_admin_value[t%d]", i), v))
		// Une variable lue « $… » relaie, à l'exécution, celle de
		// l'environnement du maître (D115) : -tt montre ce qui a été lu.
		fmt.Fprintf(&conf, "env[E%d] = %s\n", i, FPMValue("env", v))
	}
	fmt.Fprintf(&conf, "php_admin_value[sm] = %s\n", FPMValue("php_admin_value[sm]", `"/usr/sbin/sendmail -t -i"`))
	path := filepath.Join(dir, "t.conf")
	if err := os.WriteFile(path, []byte(conf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "-tt", "-y", path, "-p", dir).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "test is successful") {
		t.Fatalf("php-fpm refuse le pool rendu : %v\n%s\n%s", err, out, conf.String())
	}
	read := map[string]string{}
	re := regexp.MustCompile(`NOTICE:\s+((?:php_admin_value|env)\[[^\]]+\]) = (.*)$`)
	for _, line := range strings.Split(string(out), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			read[m[1]] = m[2]
		}
	}
	for i, v := range valeursLitterales {
		for _, k := range []string{fmt.Sprintf("php_admin_value[t%d]", i), fmt.Sprintf("env[E%d]", i)} {
			if got, ok := read[k]; !ok || got != v {
				t.Errorf("%s : FPM lit %q, la déclaration porte %q", k, got, v)
			}
		}
	}
	if got := read["php_admin_value[sm]"]; got != "/usr/sbin/sendmail -t -i" {
		t.Errorf("sendmail_path cité par l'exploitant : FPM lit %q", got)
	}
}
