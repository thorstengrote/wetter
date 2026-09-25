#!/system/bin/sh
#
# Wandtablet: Kiosk, Helligkeit, naechtlicher Selbsttest.
#
# Ersetzt die frueheren Automate-Flows "Brightness" und "Restart firefox".
# Laeuft als Dienst von Init, also als root und ohne adb. Damit braucht das
# Tablet fuer den Alltag keinen Zugriff von aussen mehr.
#
# Warum Firefox jedes Mal neu gestartet wird statt nur zu tippen: die
# Kioskseite holt sich das Vollbild nur aus einer echten Nutzergeste, und
# ihren Abfang-Klick armiert sie nur bei frisch geladener Seite. Ein Tipp auf
# eine bereits laufende Seite landet sonst in der Karte. Ein Neustart ist
# eindeutig und raeumt nebenbei den Speicher des Browsers auf.

# Die Seite kommt vom Tablet selbst, siehe /odm/etc/wandsolar.sh. Ueber
# GitHub Pages ginge es auch, dann koennte die Seite aber die Messwerte
# nicht lesen: Firefox verweigert einem HTTPS-Dokument den Abruf eines
# HTTP-Ziels, auch auf 127.0.0.1.
URL=http://127.0.0.1:8099/
FF=org.mozilla.firefox

HELL_TAG=153          # 60 Prozent, laut Messung +149 mA Bilanz
HELL_NACHT=51         # 20 Prozent, laut Messung +390 mA Bilanz
TAG_AB=300            # 05:00 in Minuten seit Mitternacht
NACHT_AB=1410         # 23:30
PRUEFUNG=180          # 03:00

CPU_GRENZE=35         # Prozent
MEM_GRENZE=400000     # kB frei

L=/data/local/tmp/wandwacht.log
B=/data/local/tmp/bilanz.log

sag(){
    echo "$(date '+%m-%d %H:%M') $*" >> $L
    if [ "$(wc -c < $L)" -gt 200000 ]; then
        tail -400 $L > $L.neu && mv $L.neu $L
    fi
}

minuten(){ echo $(( 10#$(date +%H) * 60 + 10#$(date +%M) )); }

# Ladebilanz alle zehn Minuten, gemittelt ueber fuenf Proben.
#
# Frueher stand hier die Steigung des Charge counter aus dumpsys battery, weil
# ich die Messung rootlos halten wollte. Der Zaehler bedeutet auf diesem Geraet
# aber nicht durchgehend dasselbe, das Vorzeichen kippte. Dieser Dienst laeuft
# ohnehin als root und liest den Ladestrom deshalb direkt. Positiv heisst
# laden, negativ heisst zehren.
STROM=/sys/class/power_supply/Battery/current_now
bilanz(){
    n=0; s=0
    while [ $n -lt 5 ]; do
        n=$((n + 1))
        s=$((s + $(cat $STROM 2>/dev/null || echo 0)))
        sleep 2
    done
    D=$(dumpsys battery)
    echo "$(date '+%m-%d %H:%M')  $(echo "$D" | grep ' level:' | tr -dc 0-9)%  $(echo "$D" | grep ' voltage:' | tr -dc 0-9) mV  Hell $(settings get system screen_brightness)  Strom $((s / 5)) mA" >> $B
    if [ "$(wc -c < $B)" -gt 200000 ]; then
        tail -400 $B > $B.neu && mv $B.neu $B
    fi
}

#
# Firefox merkt sich seine Tabs und oeffnet fuer jede VIEW-Absicht einen neuen.
# Ohne Aufraeumen sammelt sich mit jedem Neustart ein Tab an; am 25.09.2026
# waren es siebzehn. Schlimmer als der Speicherverbrauch: beim Start kommt der
# zuletzt aktive Tab nach vorn, und das war dann eine alte Fassung der Seite.
# Ein Rollout sah deshalb aus, als haette er nicht gewirkt.
#
SITZUNG=/data/data/org.mozilla.firefox/files/mozilla_components_session_storage_gecko.json
LETZTE=/data/data/org.mozilla.firefox/files/mozac.feature.recentlyclosed

kiosk(){
    input keyevent 224                       # aufwecken, falls der Schirm aus ist
    am force-stop $FF
    sleep 3
    rm -f "$SITZUNG" 2>/dev/null
    rm -rf "$LETZTE" 2>/dev/null
    am start -n $FF/org.mozilla.fenix.IntentReceiverActivity \
             -a android.intent.action.VIEW -d "$URL" >/dev/null 2>&1
    sleep 25
    input tap 600 960                        # erster Klick geht ins Vollbild
    sag "Kiosk gestartet"
}

hell(){
    M=$(minuten)
    if [ "$M" -ge $TAG_AB ] && [ "$M" -lt $NACHT_AB ]; then W=$HELL_TAG; else W=$HELL_NACHT; fi
    IST=$(settings get system screen_brightness)
    if [ "$IST" != "$W" ]; then
        settings put system screen_brightness $W
        sag "Helligkeit $IST -> $W"
    fi
}

cpu(){
    A=$(head -1 /proc/stat); sleep 10; B=$(head -1 /proc/stat)
    set -- $A; ai=$(($5+$6)); at=0; for v in $2 $3 $4 $5 $6 $7 $8; do at=$((at+v)); done
    set -- $B; bi=$(($5+$6)); bt=0; for v in $2 $3 $4 $5 $6 $7 $8; do bt=$((bt+v)); done
    echo $(( 100 - (bi-ai)*100/(bt-at) ))
}

while [ "$(getprop sys.boot_completed)" != "1" ]; do sleep 5; done
sleep 30

# Das Protokoll gehoert root. Ohne diese Freigabe waere es nur mit root zu
# lesen, und genau das wollen wir auf diesem Geraet vermeiden.
touch $L; chmod 666 $L
touch $B; chmod 666 $B

settings put system screen_off_timeout 2147483647
settings put system screen_brightness_mode 0
sag "Start nach Neustart"
hell
kiosk

GEPRUEFT=
RUNDE=0
bilanz
while true; do
    sleep 60
    hell

    RUNDE=$((RUNDE + 1))
    if [ $RUNDE -ge 10 ]; then RUNDE=0; bilanz; fi

    if [ -z "$(pidof $FF)" ]; then
        sag "Firefox war weg"
        kiosk
    fi

    M=$(minuten)
    HEUTE=$(date +%Y%m%d)
    if [ "$M" -ge $PRUEFUNG ] && [ "$M" -lt $((PRUEFUNG + 5)) ] && [ "$GEPRUEFT" != "$HEUTE" ]; then
        GEPRUEFT=$HEUTE
        C=$(cpu)
        F=$(grep MemAvailable /proc/meminfo | tr -dc 0-9)
        sag "Nachtpruefung: CPU ${C} Prozent, frei $((F/1024)) MB"
        if [ "$C" -gt $CPU_GRENZE ] || [ "$F" -lt $MEM_GRENZE ]; then
            sag "Grenzwert ueberschritten, Neustart"
            sync
            reboot
            sleep 120
        else
            kiosk
        fi
    fi
done
