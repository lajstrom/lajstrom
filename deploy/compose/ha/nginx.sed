# The nginx of the ha profile runs deploy/nginx/lajstrom.conf through this script, and these
# lines are all the differences: the two replicas as the upstream, and plain HTTP on port 80
# instead of TLS on 443 (the dev stack's externalURL is http://localhost:<port>).
s/^\( *\)server lajstrom:8080; .*/\1server lajstrom-a:8080;\
\1server lajstrom-b:8080;/
s/^\( *\)listen 443 ssl;/\1listen 80;/
/^ *ssl_certificate/d
