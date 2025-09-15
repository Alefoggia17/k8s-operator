# main.py

import pandas as pd
from functions_dataset import (
    load_and_clean,
    segment_and_group,
    calculate_and_sort_means,
    apply_reductions,
    simula_settimana,
    build_df_from_slots,
    espandi_settimane,
    save_to_csv
)

# === PARAMETRI ===
FILE_PATH = "CPUUsage.csv"
RANGES = [
    (0, 721), (722, 1491), (1492, 1852),
    (1853, 2213), (2214, 2574), (2575, 2935),
    (2936, 3118), (3119, 3299)
]
UTENTI_ASSOC = [5, 11, 16, 23, 27, 31, 38, 49]
REDUCTION_MAP = {3: 0.35, 5: 0.35, 7: 0.35}
GIORNI_SETTIMANA = ['Lun', 'Mar', 'Mer', 'Gio', 'Ven', 'Sab', 'Dom']
UTENTI_POSSIBILI = [5, 11, 16, 23, 27]
UTENTI_WEEKEND = [23, 27, 31, 38, 49]
SETTIMANE_DA_SIMULARE = 5
OUTPUT_CSV = "dati_settimane.csv"


def main():
    # 1. Caricamento e pulizia
    df = load_and_clean(FILE_PATH)

    # 2. Segmentazione e raggruppamento
    grouped_dfs = segment_and_group(df, RANGES)

    # 3. Calcolo medie e ordinamento
    media_sorted, grouped_sorted, utenti_sorted = calculate_and_sort_means(grouped_dfs, UTENTI_ASSOC)

    # 4. Applicazione riduzioni specifiche
    apply_reductions(grouped_dfs, REDUCTION_MAP)

    # 5. Simulazione settimana base (slot variabili + curve)
    mu, slot_durata = simula_settimana(
        num_settimane=1,
        giorni_settimana=GIORNI_SETTIMANA,
        utenti_possibili=UTENTI_POSSIBILI,
        utenti_weekend=UTENTI_WEEKEND,
        sigma=0.5
    )

    # 6. Costruzione DataFrame slot
    df_slot = build_df_from_slots(mu, slot_durata, GIORNI_SETTIMANA)
    print("\nEsempio slot settimana 1:")
    print(df_slot.head(10).to_string(index=False))

    # 7. Espansione su più settimane
    df_expanded = espandi_settimane(df_slot, grouped_sorted, utenti_sorted, settimane=SETTIMANE_DA_SIMULARE)

    # 8. Salvataggio finale
    save_to_csv(df_expanded, OUTPUT_CSV)


if __name__ == "__main__":
    main()
