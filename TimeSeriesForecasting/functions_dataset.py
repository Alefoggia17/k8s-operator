import pandas as pd
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.dates as mdates
import matplotlib.colors as mcolors
from tabulate import tabulate
import matplotlib.cm as cm
import matplotlib.ticker as ticker

# ==============================
# === 1. DATA LOADING & CLEAN ===
# ==============================
def load_and_clean(file_path: str) -> pd.DataFrame:
    df = pd.read_csv(file_path)

    # Rimuovi colonne con NaN
    cols_with_nan = df.columns[df.isna().any()].tolist()
    df = df.drop(columns=cols_with_nan)
    if cols_with_nan:
        print("Colonne rimosse per presenza di NaN:")
        for col in cols_with_nan:
            print(f"  - {col}")

    df['Time'] = pd.to_datetime(df['Time'])
    print(f"Numero colonne rimanenti: {df.shape[1]}")
    return df


# =================================
# === 2. SEGMENTAZIONE & GROUP ===
# =================================
def segment_and_group(df: pd.DataFrame, ranges: list) -> list:
    grouped_dataframes = []
    for i, (start, end) in enumerate(ranges):
        segment = df.iloc[start:end + 1].copy()
        segment['Minute'] = segment['Time'].dt.floor('T')
        grouped = segment.groupby('Minute').mean(numeric_only=True).reset_index()

        zero_cols = [c for c in grouped.columns if c != 'Minute' and (grouped[c] == 0).all()]
        if zero_cols:
            print(f"DF {i + 1}: colonne rimosse (solo zeri): {zero_cols}")
            grouped = grouped.drop(columns=zero_cols)

        print(f"\nDF {i + 1} ({start+1}–{end+1}) → Orig: {len(segment)} | Raggruppato: {len(grouped)}")
        print(tabulate(grouped.head(3), headers='keys', tablefmt='pretty'))

        grouped_dataframes.append(grouped)
    return grouped_dataframes


# ==========================================
# === 3. CALCOLO MEDIE & ORDINAMENTO ===
# ==========================================
def calculate_and_sort_means(grouped_dfs: list, utenti_assoc: list):
    media_totale = []
    for i, dfg in enumerate(grouped_dfs):
        dfg_clean = dfg.dropna(axis=1, how='all')
        numeric_cols = dfg_clean.select_dtypes(include='number').columns
        media_val = dfg_clean[numeric_cols].mean().mean()
        media_totale.append((i, media_val, dfg_clean))

    media_totale_sorted = sorted(media_totale, key=lambda x: x[1])
    utenti_assoc_ordinati = sorted(utenti_assoc)[:len(media_totale_sorted)]
    grouped_sorted = [item[2] for item in media_totale_sorted]

    for pos, (idx, media_val, _) in enumerate(media_totale_sorted, 1):
        print(f"Pos {pos}: DF {idx+1} - Media {media_val:.4f} - Utenti {utenti_assoc_ordinati[pos-1]}")

    return media_totale_sorted, grouped_sorted, utenti_assoc_ordinati


# ==========================================
# === 4. RIDUZIONE SPECIFICA SU DF ===
# ==========================================
def reduce_and_clip_percent(df: pd.DataFrame, percent: float):
    factor = 1 - percent
    numeric_cols = df.select_dtypes(include='number').columns
    df[numeric_cols] = df[numeric_cols] * factor
    df[numeric_cols] = df[numeric_cols].clip(lower=0)


def apply_reductions(grouped_dfs: list, reduction_map: dict):
    for df_num, reduction_percent in reduction_map.items():
        idx = df_num - 1
        reduce_and_clip_percent(grouped_dfs[idx], reduction_percent)
        print(f"RIDUZIONE APPLICATA DF {df_num} (-{int(reduction_percent*100)}%)")


# ===================================
# === 5. GENERAZIONE SLOT UTENTI ===
# ===================================
def genera_slot_variabili(possibili_durata=[30, 45, 60], minuti_target=1440):
    slot, rimanenti = [], minuti_target
    while rimanenti > 0:
        durate_possibili = [d for d in possibili_durata if d <= rimanenti]
        if not durate_possibili:
            return genera_slot_variabili(possibili_durata, minuti_target)
        durata = np.random.choice(durate_possibili)
        slot.append(durata)
        rimanenti -= durata
    return slot


def calcola_centro_slot(slot):
    centro, t = [], 0
    for durata in slot:
        centro.append(t + durata/2)
        t += durata
    return np.array(centro)


def minuti_to_orario(minuti):
    return f"{int(minuti//60):02d}:{int(minuti%60):02d}"


# ===================================
# === 6. SIMULAZIONE SETTIMANA ===
# ===================================
def simula_settimana(num_settimane, giorni_settimana, utenti_possibili, utenti_weekend, sigma=0.5):
    mu, slot_durata_settimane = [], []
    centro_picchi_base = [8, 13, 18, 22]
    centro_picchi_minuti = [x*60 for x in centro_picchi_base]

    for settimana in range(num_settimane):
        mu_sett, slot_sett = [], []
        for d, giorno in enumerate(giorni_settimana):
            slot = genera_slot_variabili()
            slot_sett.append(slot)
            centro_slot = calcola_centro_slot(slot)

            utenti = utenti_possibili if d < 5 else utenti_weekend
            num_picchi = np.random.randint(1, 5)
            picchi = np.random.normal(np.random.choice(centro_picchi_minuti, size=num_picchi, replace=False), 60)

            curve_base = np.array([
                sum(np.exp(-((c-p)**2)/(2*(sigma*60)**2)) for p in picchi)
                for c in centro_slot
            ])
            curve_base /= np.max(curve_base)

            valori = np.random.choice(utenti, size=len(curve_base))
            mu_sett.append(valori)
        mu.append(mu_sett)
        slot_durata_settimane.append(slot_sett)

    return mu, slot_durata_settimane


# ===================================
# === 7. COSTRUZIONE DATAFRAME ===
# ===================================
def build_df_from_slots(mu, slot_durata_settimane, giorni_settimana):
    rows = []
    for settimana in range(len(mu)):
        for d, giorno_nome in enumerate(giorni_settimana):
            durata_slot, utenti_slot = slot_durata_settimane[settimana][d], mu[settimana][d]
            t_inizio = 0
            for durata, utenti in zip(durata_slot, utenti_slot):
                inizio, fine = t_inizio, t_inizio + durata
                rows.append({
                    'settimana': settimana+1,
                    'giorno': giorno_nome,
                    'slot_inizio_minuti': inizio,
                    'slot_durata_minuti': durata,
                    'slot_fine_minuti': fine,
                    'orario_inizio': minuti_to_orario(inizio),
                    'orario_fine': minuti_to_orario(fine),
                    'num_utenti': utenti
                })
                t_inizio = fine
    return pd.DataFrame(rows)


# ===================================
# === 8. GENERAZIONE ESPANSIONE ===
# ===================================
def espandi_settimane(df_slot, grouped_sorted, utenti_assoc_ordinati, settimane=5):
    utente_to_df = dict(zip(utenti_assoc_ordinati, grouped_sorted))
    records = []
    start_time = pd.Timestamp.now().normalize() + pd.Timedelta((7 - pd.Timestamp.now().weekday()) % 7, unit='D')

    for w in range(settimane):
        week_start = start_time + pd.Timedelta(weeks=w)
        for i, row in df_slot.iterrows():
            giorno_offset = ['Lun','Mar','Mer','Gio','Ven','Sab','Dom'].index(row['giorno'])
            inizio_slot = week_start + pd.Timedelta(days=giorno_offset, minutes=row['slot_inizio_minuti'])
            fine_slot = week_start + pd.Timedelta(days=giorno_offset, minutes=row['slot_fine_minuti'])
            num_utenti = int(row['num_utenti'])

            df_corr = utente_to_df[num_utenti].drop(columns=['Minute'], errors='ignore')
            base_values = df_corr.iloc[i % len(df_corr)]

            for minuto in range(int(row['slot_durata_minuti'])):
                ts = inizio_slot + pd.Timedelta(minutes=minuto)
                noisy = base_values * (1 + np.random.normal(0, 0.07, size=base_values.shape))
                noisy = np.clip(noisy, 0, 100)

                rec = {'Timestamp': ts}
                rec.update(noisy.to_dict())
                records.append(rec)

    return pd.DataFrame(records)


# ===================================
# === 9. SALVATAGGIO ===
# ===================================
def save_to_csv(df: pd.DataFrame, filename: str):
    df.iloc[:, :-3].to_csv(filename, index=False)
    print(f"Salvato {filename}")
