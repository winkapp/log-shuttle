FROM heroku/log-shuttle:0.23.0-1-gabfbbf9

RUN apk update

RUN apk add wget sudo bash socat

ADD ./heroku_kinesis.sh /root/

EXPOSE 514/udp

ENTRYPOINT ["/bin/bash", "-c"]
CMD ["/root/heroku_kinesis.sh"]
