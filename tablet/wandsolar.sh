#!/system/bin/sh
# Liest die Solaranlage ueber Modbus und liefert die Wetterseite samt
# Messwerten auf 127.0.0.1:8099 aus. Siehe tablet/wandsolar/main.go im Repo
# wetter, dort steht auch, warum die Seite lokal liegen muss.
#
# Keine eigene Wiederholungsschleife: wandsolar faengt Verbindungsabbrueche
# selbst ab. Stirbt der Prozess trotzdem, startet Init den Dienst neu, weil
# in der rc-Datei kein oneshot steht.

VERZ=/data/local/tmp/wand
PROTOKOLL=$VERZ/wandsolar.log

mkdir -p "$VERZ"

# Protokoll kappen, falls es ueber die Monate zu gross geworden ist.
if [ -f "$PROTOKOLL" ]; then
    GROESSE=$(stat -c %s "$PROTOKOLL" 2>/dev/null || echo 0)
    if [ "$GROESSE" -gt 1000000 ]; then
        tail -c 100000 "$PROTOKOLL" > "$PROTOKOLL.neu" && mv "$PROTOKOLL.neu" "$PROTOKOLL"
    fi
fi

# Alle Pfade absolut: Init startet den Dienst im Wurzelverzeichnis, und das
# ist schreibgeschuetzt.
cd "$VERZ" || exit 1

exec /data/local/tmp/wandsolar \
    -wr 192.168.2.136:502 \
    -hoere 127.0.0.1:8099 \
    -seite "$VERZ/index.html" \
    -daten "$VERZ/tag.json" \
    >> "$PROTOKOLL" 2>&1
