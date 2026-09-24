# Image d'intégration : Debian avec Apache et PHP-FPM, pour éprouver ce que les
# tests unitaires ne peuvent pas — le comportement réel du serveur web.
FROM debian:13-slim

ENV DEBIAN_FRONTEND=noninteractive

# L'image « slim » exclut pages de manuel et documentation : il faut lever cette
# exclusion pour éprouver ce que le paquet installe réellement.
RUN rm -f /etc/dpkg/dpkg.cfg.d/docker

# Postfix pose une question à l'installation : sans réponse préalable, il reste
# « sans configuration », et le scénario DKIM n'aurait aucun courrier à signer.
RUN echo "postfix postfix/main_mailer_type select Local only" | debconf-set-selections \
    && echo "postfix postfix/mailname string essai.local" | debconf-set-selections

RUN apt-get update && apt-get install -y --no-install-recommends \
        apache2 \
        libapache2-mod-fcgid \
        nginx \
        php-fpm \
        curl \
        ca-certificates \
        passwd \
        acl \
        sudo \
        man-db \
        less \
        mariadb-server \
        mariadb-client \
        postgresql \
        postgresql-contrib \
        procps \
        openssl \
        openssh-client \
        systemd \
        systemd-sysv \
        cron \
        logrotate \
        python3-minimal \
        python3-pika \
        fail2ban \
        python3-systemd \
        iptables \
        opendkim \
        postfix \
        dnsmasq-base \
        python3-dkim \
        ufw \
        proftpd-core \
        proftpd-mod-crypto \
        openssh-server \
        iproute2 \
        netcat-openbsd \
    && a2enmod proxy proxy_fcgi proxy_http rewrite headers setenvif ssl status \
    && rm -rf /var/lib/apt/lists/*

# Sans init dans le conteneur, systemctl est inopérant : un substitut dispatche
# les actions sur les services réellement installés. Il s'efface de lui-même
# lorsque le scénario démarre un vrai systemd en PID 1.
COPY test/integration/systemctl-shim.sh /usr/local/bin/systemctl

# Le binaire est compilé à l'extérieur : statique, il n'a besoin de rien ici.
COPY ogs /usr/bin/ogs
COPY test/integration/scenarios /scenarios

RUN chmod +x /usr/bin/ogs /scenarios/*.sh /usr/local/bin/systemctl

ENTRYPOINT ["/bin/bash"]
