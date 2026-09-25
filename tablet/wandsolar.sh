#!/system/bin/sh
#
# wandsolar - liest die Solaranlage und liefert sie an die Wetterseite.
#
# Warum auf dem Tablet und nicht anderswo: die Wetterseite laeuft im Browser
# und kann kein Modbus. Ein Rechner im Netz koennte es, aber dann haengt die
# Wand an einem Rechner. Das Tablet ist ohnehin rund um die Uhr wach, hat
# toybox mit nc, xxd und printf, und damit reicht es fuer beides.
#
# Zwei Teile:
#   1. Ein Leser, der alle 30 Sekunden den Wechselrichter abfragt und
#      /data/local/tmp/solar.json schreibt, samt Stundenmitteln.
#   2. Ein winziger Webdienst auf 127.0.0.1:8099, der diese Datei ausliefert.
#      Die Seite kommt von GitHub Pages ueber HTTPS und duerfte kein
#      HTTP-Ziel abrufen. Ausgenommen sind localhost und 127.0.0.1, die
#      gelten seit Firefox 84 als vertrauenswuerdig. Deshalb bindet der
#      Dienst ausdruecklich nur dort und ist im Heimnetz nicht sichtbar.
#
# Aufruf:
#   wandsolar.sh            beide Teile, laeuft ewig
#   wandsolar.sh --einmal   ein Messwert auf die Standardausgabe
#   wandsolar.sh --serve    interner Modus, eine HTTP-Antwort
#
WR=192.168.2.136
MODBUS=502
WEB=8099
TAKT=30
JSON=/data/local/tmp/solar.json
STD=/data/local/tmp/solarstunden
LOG=/data/local/tmp/wandsolar.log

sag(){ echo "$(date '+%m-%d %H:%M') $*" >> $LOG; }

# ---------------------------------------------------------------- Modbus
# Eine Anfrage, eine Verbindung. Der Dongle braucht nach dem Verbindungs-
# aufbau etwa eine Sekunde Ruhe und laesst nur einen Client gleichzeitig zu.
roh(){
    a=$(printf '%04x' "$1"); n=$(printf '%04x' "$2")
    { sleep 1.4
      printf "\x00\x01\x00\x00\x00\x06\x01\x03\x${a%??}\x${a#??}\x${n%??}\x${n#??}"
      sleep 1.6
    } | toybox nc $WR $MODBUS 2>/dev/null | toybox xxd -p | tr -d '\n'
}

# 9 Byte Kopf, danach die Nutzdaten. Kuerzer heisst: nichts Brauchbares.
daten(){
    r=$(roh "$1" "$2")
    [ ${#r} -lt 20 ] && return 1
    echo "${r#??????????????????}"
}

# Vorzeichenbehaftete 32 Bit
i32(){
    h=$(daten "$1" 2) || return 1
    v=$((0x$h))
    [ $v -gt 2147483647 ] && v=$((v - 4294967296))
    echo $v
}
u32(){ h=$(daten "$1" 2) || return 1; echo $((0x$h)); }
u16(){ h=$(daten "$1" 1) || return 1; echo $((0x$h)); }

# Ganzzahl mit Nachkommastellen ausgeben, ohne Gleitkomma
komma(){   # $1 Wert, $2 Teiler als Stellenzahl (3 = durch 1000)
    v=$1; s=""
    [ "$v" -lt 0 ] && { s="-"; v=$((0 - v)); }
    case $2 in
      3) printf '%s%d.%03d' "$s" $((v/1000)) $((v%1000));;
      2) printf '%s%d.%02d' "$s" $((v/100))  $((v%100));;
      1) printf '%s%d.%d'   "$s" $((v/10))   $((v%10));;
    esac
}

# ---------------------------------------------------------------- Messung
# 32064 ist der Gleichstromeingang, also die echte Erzeugung.
# 32080 ist der Ausgang des Wechselrichters und enthaelt abends den Akku.
# Der Hausverbrauch ist Ausgang minus Einspeisung.
messe(){
    PV=$(i32 32064)   || return 1
    AUS=$(i32 32080)  || return 1
    NETZ=$(i32 37113) || return 1
    AKKU=$(i32 37765) || return 1
    SOC=$(u16 37760)  || return 1
    ERT=$(u32 32114)  || ERT=0
    GEL=$(u32 37015)  || GEL=0
    ENT=$(u32 37017)  || ENT=0
    HAUS=$((AUS - NETZ))
    # 32114 zaehlt nur den Ausgang, der Akkuinhalt fehlt darin
    HEUTE=$((ERT + GEL - ENT))
    return 0
}

# ---------------------------------------------------------------- Stunden
# Je Stunde eine Datei mit den Summen und der Anzahl der Proben. Beim
# Datumswechsel faengt die Sammlung von vorn an.
sammle(){
    h=$(date +%H); h=${h#0}; [ -z "$h" ] && h=0
    heute=$(date +%Y%m%d)
    [ -f $STD/tag ] && [ "$(cat $STD/tag)" != "$heute" ] && rm -rf $STD
    mkdir -p $STD; echo "$heute" > $STD/tag
    f=$STD/$h
    if [ -f "$f" ]; then read sp sh sa sn n < "$f"; else sp=0; sh=0; sa=0; sn=0; n=0; fi
    echo "$((sp+PV)) $((sh+HAUS)) $((sa+AKKU)) $((sn+NETZ)) $((n+1))" > "$f"
}

stundenJson(){
    erste=1; printf '{'
    i=0
    while [ $i -lt 24 ]; do
        f=$STD/$i
        if [ -f "$f" ]; then
            read sp sh sa sn n < "$f"
            if [ "$n" -gt 0 ]; then
                [ $erste -eq 0 ] && printf ','
                erste=0
                printf '"%d":{"pv":%s,"haus":%s,"akku":%s,"netz":%s}' \
                  $i "$(komma $((sp/n)) 3)" "$(komma $((sh/n)) 3)" \
                     "$(komma $((sa/n)) 3)" "$(komma $((sn/n)) 3)"
            fi
        fi
        i=$((i+1))
    done
    printf '}'
}

schreibe(){
    {
      printf '{"zeit":"%s","jetzt":{"pv":%s,"haus":%s,"akku":%s,"netz":%s,"soc":%s},' \
        "$(date '+%Y-%m-%dT%H:%M:%S')" \
        "$(komma $PV 3)" "$(komma $HAUS 3)" "$(komma $AKKU 3)" \
        "$(komma $NETZ 3)" "$(komma $SOC 1)"
      printf '"heute_kwh":%s,"stunden":' "$(komma $HEUTE 2)"
      stundenJson
      printf '}'
    } > $JSON.neu && mv $JSON.neu $JSON
    chmod 644 $JSON 2>/dev/null
}

# ---------------------------------------------------------------- Webdienst
# Wird von nc -L je Verbindung aufgerufen. Erst die Anfrage leeren, damit
# der Browser keine abgebrochene Verbindung sieht, dann antworten.
serve(){
    while IFS= read -r zeile; do
        z=$(echo "$zeile" | tr -d '\r')
        [ -z "$z" ] && break
    done
    printf 'HTTP/1.1 200 OK\r\n'
    printf 'Content-Type: application/json; charset=utf-8\r\n'
    printf 'Access-Control-Allow-Origin: *\r\n'
    printf 'Cache-Control: no-store\r\n'
    printf 'Connection: close\r\n\r\n'
    if [ -f $JSON ]; then cat $JSON; else printf '{}'; fi
}

# ---------------------------------------------------------------- Start
case "$1" in
  --serve)
    serve; exit 0;;
  --einmal)
    if messe; then
        echo "PV $(komma $PV 3) kW  Haus $(komma $HAUS 3) kW  Netz $(komma $NETZ 3) kW  Akku $(komma $SOC 1) %  heute $(komma $HEUTE 2) kWh"
    else
        echo "keine Antwort vom Wechselrichter"; exit 1
    fi
    exit 0;;
esac

sag "wandsolar startet"
# Webdienst im Hintergrund, nc -L nimmt beliebig viele Verbindungen an
( while true; do
    toybox nc -s 127.0.0.1 -p $WEB -L sh "$0" --serve 2>>$LOG
    sleep 2
  done ) &

fehler=0
while true; do
    if messe; then
        sammle
        schreibe
        [ $fehler -gt 0 ] && sag "wieder Antwort nach $fehler Fehlversuchen"
        fehler=0
    else
        fehler=$((fehler+1))
        [ $fehler -eq 1 ] && sag "keine Antwort vom Wechselrichter"
        [ $fehler -gt 20 ] && { sag "seit $fehler Versuchen still"; fehler=2; }
    fi
    sleep $TAKT
done
