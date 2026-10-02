# Sourced by model-cache-fill.sh and model-upload.sh (ADR 0019).
# model_sums <model>: print "<sha256>  <path>" for every file
# models/checksums.txt lists for <model>, with the path relative to the
# models root: <model>/<file>, or <model> for a single-file model.
model_sums() {
  found=
  while read -r sum path; do
    case "$sum" in '#'*|'') continue ;; esac
    case "$path" in
      "models/$1"|"models/$1/"*)
        printf '%s  %s\n' "$sum" "${path#models/}"
        found=1
        ;;
    esac
  done < /scripts/checksums.txt
  [ -n "$found" ] || { echo "no entries for $1 in models/checksums.txt" >&2; return 1; }
}
