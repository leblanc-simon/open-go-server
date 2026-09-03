package monitoring

import (
	"context"
	"fmt"
	"strings"

	"leblanc.io/open-go-server/internal/exec"
)

// ReloadFail2ban revalide puis recharge le démon.
//
// Le contrôle préalable n'est pas une politesse : une prison dont fail2ban ne
// trouve aucun journal l'empêche de démarrer, et il s'arrête alors entièrement —
// les prisons valides comprises. C'est le même risque que pour le serveur web, et
// il appelle la même prudence : on valide avant, et on constate après.
func (p *Provider) ReloadFail2ban(ctx context.Context) error {
	if p.Exec == nil || p.Exec.Simulated() {
		return nil
	}
	if res, err := p.Exec.Run(ctx, exec.Command{Name: "fail2ban-client", Args: []string{"-t"}}); err != nil {
		return fmt.Errorf("configuration fail2ban refusée : %s", firstError(res, err))
	}
	if !p.unitActive(ctx, "fail2ban") {
		// Le démon est arrêté : le recharger n'a pas de sens, et le démarrer serait
		// une décision que l'administrateur n'a pas prise.
		return nil
	}
	res, err := p.Exec.Run(ctx, exec.Command{Name: "fail2ban-client", Args: []string{"reload"}})
	if err != nil {
		return fmt.Errorf("rechargement de fail2ban : %s", firstError(res, err))
	}
	if !p.unitActive(ctx, "fail2ban") {
		// Le constat en aval : une prison peut être refusée au rechargement alors
		// que le contrôle préalable l'acceptait — un journal supprimé entre les
		// deux suffit. On relève le démon, et on le dit même si la relève réussit.
		if _, err := p.Exec.Run(ctx, exec.Command{
			Name: "systemctl", Args: []string{"restart", "fail2ban"}}); err != nil {
			return fmt.Errorf("fail2ban s'est arrêté au rechargement et n'a pas pu être relancé : %w", err)
		}
		return fmt.Errorf("fail2ban s'est arrêté au rechargement ; il a été relancé — " +
			"vérifier /var/log/fail2ban.log")
	}
	return nil
}

// ReloadTelegraf recharge l'agent de métriques, puis constate.
//
// Il n'y a pas de contrôle préalable, faute d'équivalent d'« apachectl -t » :
// l'option de test de Telegraf exécute réellement une collecte, ce qui la rend
// impropre à une validation — elle échouerait sur un moteur momentanément
// injoignable, et retarderait l'apply de tous ses délais d'attente. Le constat en
// aval reste, et il suffit : un agent mort ne fait tomber aucun site, il fait
// taire la supervision. Encore faut-il le dire.
func (p *Provider) ReloadTelegraf(ctx context.Context) error {
	if p.Exec == nil || p.Exec.Simulated() {
		return nil
	}
	if !p.unitActive(ctx, "telegraf") {
		return nil
	}
	if res, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"reload-or-restart", "telegraf"}}); err != nil {
		return fmt.Errorf("rechargement de telegraf : %s", firstError(res, err))
	}
	if !p.unitActive(ctx, "telegraf") {
		return fmt.Errorf("telegraf s'est arrêté au rechargement : " +
			"vérifier « journalctl -u telegraf » — les entrées écrites par ogs sont en cause " +
			"si l'arrêt suit immédiatement l'apply")
	}
	return nil
}

// unitActive interroge systemd. Une unité inconnue n'est pas une erreur : elle
// signifie que l'outil n'est pas installé sous ce nom, et le rechargement se
// borne alors à ne rien faire.
func (p *Provider) unitActive(ctx context.Context, unit string) bool {
	res, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"is-active", unit}})
	if err != nil && res.Stdout == "" {
		return false
	}
	return strings.TrimSpace(res.Stdout) == "active"
}

// firstError privilégie la sortie d'erreur de la commande, plus parlante que le
// code de retour qu'elle a produit.
func firstError(res exec.Result, err error) string {
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return s
	}
	if s := strings.TrimSpace(res.Stdout); s != "" {
		return s
	}
	return err.Error()
}
