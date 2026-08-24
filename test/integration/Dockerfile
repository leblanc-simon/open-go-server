# Image d'intégration : Debian avec Apache et PHP-FPM, pour éprouver ce que les
# tests unitaires ne peuvent pas — le comportement réel du serveur web.
FROM debian:13-slim

ENV DEBIAN_FRONTEND=noninteractive

# L'image « slim » exclut pages de manuel et documentation : il faut lever cette
# exclusion pour éprouver ce que le paquet installe réellement.
RUN rm -f /etc/dpkg/dpkg.cfg.d/docker

RUN apt-get update && apt-get install -y --no-install-recommends \
        apache2 \
        libapache2-mod-fcgid \
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
        python3-minimal \
        python3-pika \
    && a2enmod proxy proxy_fcgi rewrite headers setenvif ssl \
    && rm -rf /var/lib/apt/lists/*

# Sans init dans le conteneur, systemctl n'existe pas : un substitut dispatche
# les actions sur les services réellement installés.
COPY test/integration/systemctl-shim.sh /usr/local/bin/systemctl

# Le binaire est compilé à l'extérieur : statique, il n'a besoin de rien ici.
COPY ogs /usr/bin/ogs
COPY test/integration/scenarios /scenarios

RUN chmod +x /usr/bin/ogs /scenarios/*.sh /usr/local/bin/systemctl

ENTRYPOINT ["/bin/bash"]
