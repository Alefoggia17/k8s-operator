# =========================================================
# TIME SERIES FORECASTING - Conv + LSTM con Residual Blocks
# =========================================================

# === 0. Import delle librerie ===
import os
import random
import numpy as np
import pandas as pd
import matplotlib.pyplot as plt
import matplotlib.dates as mdates

from itertools import combinations
from sklearn.preprocessing import MinMaxScaler
from sklearn.metrics import r2_score, mean_squared_error, mean_absolute_error

import torch
import torch.nn as nn
import torch.nn.functional as F
from torch.utils.data import DataLoader, TensorDataset
import torch_optimizer as optim  # Ottimizzatori avanzati (es. RAdam)

from google.colab import drive


# === 1. Monta Google Drive ===
drive.mount('/content/drive')


# === 2. Caricamento dataset ===
df = pd.read_csv("/content/drive/MyDrive/df_finale_5_settimane_high.csv")

# Conversione e feature engineering temporale
df["Time"] = pd.to_datetime(df["Timestamp"])
df["hour"] = df["Time"].dt.hour
df["weekday"] = df["Time"].dt.weekday


# === 3. Configurazioni generali ===
window_size = 1380        # Lunghezza finestra input
output_steps = 60         # Numero step futuri da predire
samples_per_day = 1440
train_days = 28
test_days = 7

train_len = train_days * samples_per_day
test_len = test_days * samples_per_day

base_output_path = "/content/drive/MyDrive/modelli_high"
os.makedirs(base_output_path, exist_ok=True)

device = torch.device("cuda" if torch.cuda.is_available() else "cpu")


# === 4. Definizione modelli ===
class ResidualBlock(nn.Module):
    """Blocco residuo per stabilizzare la CNN 1D"""
    def __init__(self, channels):
        super().__init__()
        self.conv1 = nn.Conv1d(channels, channels, kernel_size=3, padding=1)
        self.bn1 = nn.BatchNorm1d(channels)
        self.conv2 = nn.Conv1d(channels, channels, kernel_size=3, padding=1)
        self.bn2 = nn.BatchNorm1d(channels)

    def forward(self, x):
        residual = x
        out = F.relu(self.bn1(self.conv1(x)))
        out = self.bn2(self.conv2(out))
        out += residual
        return F.relu(out)


class ConvLSTMNetImproved(nn.Module):
    """CNN + Residual Blocks + LSTM bidirezionale"""
    def __init__(self, window_size):
        super().__init__()
        # Primo blocco
        self.conv1 = nn.Conv1d(3, 64, kernel_size=3, padding=1)
        self.bn1 = nn.BatchNorm1d(64)
        self.resblock1 = ResidualBlock(64)
        self.pool1 = nn.MaxPool1d(2)
        self.drop1 = nn.Dropout(0.3)

        # Secondo blocco
        self.conv2 = nn.Conv1d(64, 128, kernel_size=3, padding=1)
        self.bn2 = nn.BatchNorm1d(128)
        self.resblock2 = ResidualBlock(128)
        self.pool2 = nn.MaxPool1d(2)
        self.drop2 = nn.Dropout(0.3)

        # LSTM bidirezionale
        self.lstm1 = nn.LSTM(128, 128, batch_first=True, bidirectional=True)
        self.lstm2 = nn.LSTM(256, 128, batch_first=True)

        # Dimensione finale dopo pooling
        final_len = 128 * (((((window_size) // 2) // 2)))

        # Fully connected
        self.fc1 = nn.Linear(final_len, 100)
        self.fc2 = nn.Linear(100, 1)

    def forward(self, x):
        # Input: (batch, time, features=3)
        x = x.permute(0, 2, 1)  

        x = F.relu(self.bn1(self.conv1(x)))
        x = self.resblock1(x)
        x = self.pool1(x)
        x = self.drop1(x)

        x = F.relu(self.bn2(self.conv2(x)))
        x = self.resblock2(x)
        x = self.pool2(x)
        x = self.drop2(x)

        x = x.permute(0, 2, 1)  
        x, _ = self.lstm1(x)
        x, _ = self.lstm2(x)

        x = x.flatten(start_dim=1)
        x = F.relu(self.fc1(x))
        return self.fc2(x)


# === 5. Funzioni utili ===
def create_sequences_single_step(valori, ore, giorni, window_size):
    """Crea sequenze con target a 1 step in avanti"""
    X, y = [], []
    for i in range(len(valori) - window_size - 1):
        v = valori[i:i+window_size]
        h = ore[i:i+window_size]
        d = giorni[i:i+window_size]
        X.append(np.stack([v, h, d], axis=-1))
        y.append(valori[i+window_size])
    return np.array(X), np.array(y)


def find_peaks(series, threshold):
    """Trova picchi oltre una soglia"""
    return series[series > threshold]


def match_peaks(pred_peaks, real_peaks, time_tolerance=pd.Timedelta(minutes=4), value_tolerance=0.1):
    """Abbina picchi predetti e reali con tolleranza temporale e di valore"""
    matched = []
    used_real_indices = set()
    for t_pred, v_pred in pred_peaks.items():
        real_candidates = real_peaks[(real_peaks.index >= t_pred - time_tolerance) & (real_peaks.index <= t_pred + time_tolerance)]
        if real_candidates.empty:
            matched.append(False)
            continue

        relative_diffs = np.abs(real_candidates.values - v_pred) / real_candidates.values
        valid_indices = real_candidates.index[relative_diffs <= value_tolerance]

        found_match = any(idx not in used_real_indices for idx in valid_indices)
        if found_match:
            used_real_indices.update(valid_indices)
        matched.append(found_match)

    return matched


# === 6. Definizione colonne da predire ===
excluded_columns = [
    "Timestamp", "hour", "weekday", "Time",
    # Tutti i servizi esclusi...
]
columns_to_predict = [col for col in df.columns if col not in excluded_columns]


# === 7. Training e validazione per ogni colonna ===
metriche_modelli = []

for col in columns_to_predict:
    print(f"\n➡️ Lavorando sulla colonna: {col}")
    output_dir = os.path.join(base_output_path, col)
    os.makedirs(output_dir, exist_ok=True)

    # Scaling
    scaler_val = MinMaxScaler()
    valori_scaled = scaler_val.fit_transform(df[col].values.reshape(-1, 1)).flatten()
    scaler_hour = MinMaxScaler()
    hours_scaled = scaler_hour.fit_transform(df["hour"].values.reshape(-1, 1)).flatten()
    scaler_week = MinMaxScaler()
    weekdays_scaled = scaler_week.fit_transform(df["weekday"].values.reshape(-1, 1)).flatten()

    # Train set
    X_train, y_train = create_sequences_single_step(
        valori_scaled[:train_len], hours_scaled[:train_len], weekdays_scaled[:train_len], window_size
    )

    # Test set
    offset = np.random.randint(1, samples_per_day)
    test_start = train_len - window_size - output_steps + offset
    test_end = test_start + test_len

    segment_len = test_end - test_start
    if segment_len <= window_size + 1:
        print(f"❌ Impossibile creare sequenze per {col}, salto.")
        continue

    # Validation set
    X_val, y_val = create_sequences_single_step(
        valori_scaled[test_start:test_end],
        hours_scaled[test_start:test_end],
        weekdays_scaled[test_start:test_end],
        window_size
    )
    if len(X_val) == 0:
        print(f"⚠️ Set di validazione vuoto per {col}, salto.")
        continue

    # Conversione in tensori
    X_train, y_train = torch.tensor(X_train, dtype=torch.float32), torch.tensor(y_train, dtype=torch.float32).unsqueeze(-1)
    X_val, y_val = torch.tensor(X_val, dtype=torch.float32), torch.tensor(y_val, dtype=torch.float32).unsqueeze(-1)

    train_loader = DataLoader(TensorDataset(X_train, y_train), batch_size=32, shuffle=True)
    val_loader = DataLoader(TensorDataset(X_val, y_val), batch_size=32)

    # Modello
    model = ConvLSTMNetImproved(window_size).to(device)
    criterion = nn.MSELoss()
    optimizer = optim.RAdam(model.parameters(), lr=0.001)

    train_losses, val_losses = [], []
    best_val_loss = float("inf")
    best_model_state = None
    patience, counter = 5, 0

    # Training loop
    for epoch in range(30):
        model.train()
        total_loss = 0
        for xb, yb in train_loader:
            xb, yb = xb.to(device), yb.to(device)
            optimizer.zero_grad()
            preds = model(xb)
            loss = criterion(preds, yb)
            loss.backward()
            optimizer.step()
            total_loss += loss.item()

        avg_train_loss = total_loss / len(train_loader)
        train_losses.append(avg_train_loss)

        model.eval()
        total_val_loss = 0
        with torch.no_grad():
            for xb, yb in val_loader:
                xb, yb = xb.to(device), yb.to(device)
                preds = model(xb)
                total_val_loss += criterion(preds, yb).item()
        avg_val_loss = total_val_loss / len(val_loader)
        val_losses.append(avg_val_loss)

        print(f"Epoch {epoch+1}/30 - Train Loss: {avg_train_loss:.6f} - Val Loss: {avg_val_loss:.6f}")

        if avg_val_loss < best_val_loss:
            best_val_loss = avg_val_loss
            best_model_state = model.state_dict()
            counter = 0
        else:
            counter += 1
            if counter >= patience:
                print(f"⏹️ Early stopping alla epoca {epoch+1}")
                break

    # Ripristina best model
    if best_model_state:
        model.load_state_dict(best_model_state)

    # Salva curva loss
    plt.figure(figsize=(10, 4))
    plt.plot(train_losses, label="Train Loss")
    plt.plot(val_losses, label="Validation Loss")
    plt.title(f"Loss Curve - {col}")
    plt.xlabel("Epoch")
    plt.ylabel("MSE")
    plt.legend()
    plt.grid(True)
    plt.savefig(os.path.join(output_dir, f"loss_{col}.png"))
    plt.close()

    # === Previsione multi-step autoregressiva ===
    start_pred = test_start
    pred_input_v = list(valori_scaled[start_pred - window_size:start_pred])
    pred_input_h = list(hours_scaled[start_pred - window_size:start_pred])
    pred_input_d = list(weekdays_scaled[start_pred - window_size:start_pred])
    predicted_scaled = []

    for step in range(output_steps):
        input_seq = np.stack(
            [pred_input_v[-window_size:], pred_input_h[-window_size:], pred_input_d[-window_size:]], axis=-1
        ).reshape(1, window_size, 3)
        input_tensor = torch.tensor(input_seq, dtype=torch.float32).to(device)
        with torch.no_grad():
            pred = model(input_tensor).cpu().numpy().flatten()[0]
        predicted_scaled.append(pred)
        pred_input_v.append(pred)
        next_idx = start_pred + step
        if next_idx < len(hours_scaled):
            pred_input_h.append(hours_scaled[next_idx])
            pred_input_d.append(weekdays_scaled[next_idx])
        else:
            pred_input_h.append(pred_input_h[-1])
            pred_input_d.append(pred_input_d[-1])

    predicted_real = scaler_val.inverse_transform(np.array(predicted_scaled).reshape(-1, 1)).flatten()
    real_values = df[col].iloc[start_pred:start_pred + output_steps].values
    last_times = df["Time"].iloc[start_pred:start_pred + output_steps]

    # Plot forecast
    plt.figure(figsize=(12, 5))
    plt.plot(last_times, real_values, label="Valori reali", linewidth=2)
    plt.plot(last_times, predicted_real, label="Previsioni", linestyle="--", color="darkred")
    plt.xlabel("Tempo")
    plt.ylabel("Valore")
    plt.title(f"Previsione ultimi {output_steps} step - {col}")
    plt.legend()
    plt.grid(True)
    plt.xticks(rotation=45)
    plt.tight_layout()
    plt.savefig(os.path.join(output_dir, f"forecast_{col}.png"))
    plt.close()

    # Metriche
    mse = mean_squared_error(real_values, predicted_real)
    mae = mean_absolute_error(real_values, predicted_real)
    r2 = r2_score(real_values, predicted_real)
    print(f"📊 Test MSE: {mse:.4f} - MAE: {mae:.4f} - R²: {r2:.4f}")

    metriche_modelli.append({"colonna": col, "mse": mse, "mae": mae, "r2": r2})

    # Salvataggio modello
    torch.save(model.state_dict(), os.path.join(output_dir, f"model_{col}.pt"))
    print(f"✅ Completato: modello e grafici salvati per {col}")


# === 8. Salva metriche complessive ===
df_metriche = pd.DataFrame(metriche_modelli)
df_metriche.to_csv(os.path.join(base_output_path, "valutazione_modelli.csv"), index=False)
print("📁 File metriche salvato:", os.path.join(base_output_path, "valutazione_modelli.csv"))

# === 9. SEZIONE SUCCESSIVA ===
# - Estrazione sequenze multiple
# - Predizione con modelli salvati
# - Analisi top R²
# - Combinazioni di predizioni con metriche di picco
# (codice identico al tuo, ripulito allo stesso modo)

# === 9. Valutazione su sequenze multiple (ultimi 10 giorni) ===

best_sequences = []

for col in columns_to_predict:
    print(f"\n🔍 Analisi sequenze multiple per {col}")
    output_dir = os.path.join(base_output_path, col)

    # Carica modello allenato
    model = ConvLSTMNetImproved(window_size).to(device)
    model.load_state_dict(torch.load(os.path.join(output_dir, f"model_{col}.pt"), map_location=device))
    model.eval()

    # Normalizzazione colonne (deve usare stessi scaler)
    scaler_val = MinMaxScaler()
    valori_scaled = scaler_val.fit_transform(df[col].values.reshape(-1, 1)).flatten()
    scaler_hour = MinMaxScaler()
    hours_scaled = scaler_hour.fit_transform(df["hour"].values.reshape(-1, 1)).flatten()
    scaler_week = MinMaxScaler()
    weekdays_scaled = scaler_week.fit_transform(df["weekday"].values.reshape(-1, 1)).flatten()

    # Seleziona 5 sequenze random negli ultimi 10 giorni
    total_len = len(valori_scaled)
    start_range = total_len - 10 * samples_per_day
    for i in range(5):
        start_idx = random.randint(start_range, total_len - output_steps - window_size)
        pred_input_v = list(valori_scaled[start_idx - window_size:start_idx])
        pred_input_h = list(hours_scaled[start_idx - window_size:start_idx])
        pred_input_d = list(weekdays_scaled[start_idx - window_size:start_idx])
        predicted_scaled = []

        # Predizione autoregressiva
        for step in range(output_steps):
            input_seq = np.stack(
                [pred_input_v[-window_size:], pred_input_h[-window_size:], pred_input_d[-window_size:]],
                axis=-1
            ).reshape(1, window_size, 3)
            input_tensor = torch.tensor(input_seq, dtype=torch.float32).to(device)
            with torch.no_grad():
                pred = model(input_tensor).cpu().numpy().flatten()[0]
            predicted_scaled.append(pred)
            pred_input_v.append(pred)
            pred_input_h.append(hours_scaled[start_idx + step] if start_idx + step < len(hours_scaled) else pred_input_h[-1])
            pred_input_d.append(weekdays_scaled[start_idx + step] if start_idx + step < len(weekdays_scaled) else pred_input_d[-1])

        predicted_real = scaler_val.inverse_transform(np.array(predicted_scaled).reshape(-1, 1)).flatten()
        real_values = df[col].iloc[start_idx:start_idx + output_steps].values
        seq_times = df["Time"].iloc[start_idx:start_idx + output_steps]

        r2 = r2_score(real_values, predicted_real)
        mse = mean_squared_error(real_values, predicted_real)

        best_sequences.append({
            "colonna": col,
            "start_time": df["Time"].iloc[start_idx],
            "r2": r2,
            "mse": mse,
            "real": real_values,
            "pred": predicted_real,
            "times": seq_times
        })

# Seleziona le 5 migliori sequenze in assoluto
best_sequences_sorted = sorted(best_sequences, key=lambda x: x["r2"], reverse=True)[:5]

for i, seq in enumerate(best_sequences_sorted, 1):
    plt.figure(figsize=(12, 5))
    plt.plot(seq["times"], seq["real"], label="Valori reali", linewidth=2)
    plt.plot(seq["times"], seq["pred"], label="Previsioni", linestyle="--", color="darkred")
    plt.xlabel("Tempo")
    plt.ylabel("Valore")
    plt.title(f"Top-{i} | Colonna: {seq['colonna']} | Start: {seq['start_time']}\nR²={seq['r2']:.3f}, MSE={seq['mse']:.3f}")
    plt.legend()
    plt.grid(True)
    plt.xticks(rotation=45)
    plt.tight_layout()
    plt.savefig(os.path.join(base_output_path, f"top_{i}_{seq['colonna']}.png"))
    plt.close()

print("✅ Completata valutazione multi-sequenza e salvataggio top-5 grafici.")


# === 10. Analisi combinata delle predizioni (triplette) ===

# Seleziona 3 colonne con R² migliori
df_metriche_sorted = df_metriche.sort_values(by="r2", ascending=False)
top_cols = df_metriche_sorted.head(3)["colonna"].tolist()
print("\n🏆 Colonne migliori:", top_cols)

# Usa best_sequences_sorted come base per combinazioni
pred_dict = {seq["colonna"]: (seq["times"], seq["real"], seq["pred"]) for seq in best_sequences_sorted}

for combo in combinations(top_cols, 3):
    times = pred_dict[combo[0]][0]
    real = pred_dict[combo[0]][1]
    preds = [pred_dict[c][2] for c in combo]

    # Media delle predizioni
    avg_pred = np.mean(preds, axis=0)

    # Calcolo metriche
    r2 = r2_score(real, avg_pred)
    mse = mean_squared_error(real, avg_pred)
    mae = mean_absolute_error(real, avg_pred)

    print(f"\n🔗 Combinazione {combo}: R²={r2:.3f}, MSE={mse:.3f}, MAE={mae:.3f}")

    # Analisi picchi
    real_series = pd.Series(real, index=times)
    avg_series = pd.Series(avg_pred, index=times)
    threshold_real = real_series.mean() + 2 * real_series.std()
    threshold_pred = avg_series.mean() + 2 * avg_series.std()

    real_peaks = find_peaks(real_series, threshold_real)
    pred_peaks = find_peaks(avg_series, threshold_pred)

    matched = match_peaks(pred_peaks, real_peaks)
    tp = sum(matched)
    fp = len(matched) - tp
    fn = len(real_peaks) - tp

    precision = tp / (tp + fp) if tp + fp > 0 else 0
    recall = tp / (tp + fn) if tp + fn > 0 else 0
    f1 = (2 * precision * recall / (precision + recall)) if precision + recall > 0 else 0

    print(f"   📊 Precision: {precision:.3f}, Recall: {recall:.3f}, F1: {f1:.3f}")

    # Plot combinazione
    plt.figure(figsize=(12, 5))
    plt.plot(times, real, label="Valori reali", linewidth=2)
    plt.plot(times, avg_pred, label=f"Media {combo}", linestyle="--", color="darkred")
    plt.scatter(real_peaks.index, real_peaks.values, color="blue", marker="o", label="Picchi reali")
    plt.scatter(pred_peaks.index, pred_peaks.values, color="red", marker="x", label="Picchi predetti")
    plt.xlabel("Tempo")
    plt.ylabel("Valore")
    plt.title(f"Combinazione {combo}\nR²={r2:.3f}, Precision={precision:.3f}, Recall={recall:.3f}, F1={f1:.3f}")
    plt.legend()
    plt.grid(True)
    plt.xticks(rotation=45)
    plt.tight_layout()
    plt.savefig(os.path.join(base_output_path, f"combo_{'_'.join(combo)}.png"))
    plt.close()

print("✅ Analisi combinata completata con salvataggio grafici.")

