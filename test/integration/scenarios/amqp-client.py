"""Client AMQP du scénario RabbitMQ.

Le compte d'un projet n'a aucun droit sur l'API de management : pour éprouver ce
qu'une application fera réellement, il faut parler le protocole.
"""

import os
import sys

import pika


def connexion(vhost="shopfr"):
    creds = pika.PlainCredentials("shopfr", os.environ["OGS_AMQP_PASS"])
    return pika.BlockingConnection(pika.ConnectionParameters(
        host="rabbitmq", port=5672, virtual_host=vhost, credentials=creds))


def publier():
    try:
        conn = connexion()
    except Exception as e:
        print("ÉCHEC : connexion AMQP refusée avec les identifiants du keystore :", e)
        return 1
    ch = conn.channel()
    ch.confirm_delivery()
    try:
        ch.basic_publish(exchange="messages", routing_key="async",
                         body=b"bonjour", mandatory=True)
    except pika.exceptions.UnroutableError:
        print("ÉCHEC : message non routable — le binding « async » manque")
        return 1
    _, _, corps = ch.basic_get("async", auto_ack=True)
    if corps != b"bonjour":
        print("ÉCHEC : le message n'est pas arrivé dans la file async (%r)" % corps)
        return 1
    print("publié sur l'exchange « messages » avec la clé async, relu dans la file :",
          corps.decode())
    conn.close()

    # L'isolation doit tenir au niveau du protocole, pas seulement dans la
    # déclaration de permissions.
    try:
        connexion("/")
        print("ÉCHEC : le compte du projet accède au vhost par défaut")
        return 1
    except Exception as e:
        print("accès au vhost « / » refusé (%s), comme attendu" % type(e).__name__)
    return 0


def remplir_emails():
    conn = connexion()
    ch = conn.channel()
    for _ in range(3):
        ch.basic_publish(exchange="messages", routing_key="emails", body=b"x")
    conn.close()
    return 0


def ajouter_ttl():
    """Ajoute un argument à une file déjà créée, ce que le broker refuse."""
    chemin = "/etc/ogs/projects/shopfr.yaml"
    s = open(chemin).read()
    repere = "          x-dead-letter-routing-key: async_failed\n"
    if repere not in s:
        print("ÉCHEC : la déclaration attendue est absente, l'édition ne mordrait pas")
        print(s)
        return 1
    s = s.replace(repere, repere + "          x-message-ttl: 60000\n", 1)
    open(chemin, "w").write(s)
    return 0


ACTIONS = {"publier": publier, "remplir-emails": remplir_emails, "ajouter-ttl": ajouter_ttl}

if __name__ == "__main__":
    sys.exit(ACTIONS[sys.argv[1]]())
