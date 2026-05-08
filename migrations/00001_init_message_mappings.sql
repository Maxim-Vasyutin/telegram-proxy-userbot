-- +goose Up

CREATE TABLE message_mappings (
    id                  BIGSERIAL PRIMARY KEY,
    pair_key            TEXT NOT NULL,
    source_chat_id      BIGINT NOT NULL,
    source_message_id   BIGINT NOT NULL,
    target_chat_id      BIGINT NOT NULL,
    target_message_id   BIGINT NOT NULL,
    direction           TEXT NOT NULL
                        CHECK (direction IN ('merchant_to_support', 'support_to_merchant')),
    media_group_id      TEXT,
    account_phone       TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Поиск зеркала по source (когда пришло событие edit/delete от исходного отправителя)
CREATE INDEX idx_message_mappings_source
    ON message_mappings (source_chat_id, source_message_id);

-- Поиск исходника по target (когда пришёл reply на ретранслированное сообщение)
CREATE INDEX idx_message_mappings_target
    ON message_mappings (target_chat_id, target_message_id);

-- Группировка альбомов (требуется при удалении/edit альбома целиком)
CREATE INDEX idx_message_mappings_media_group
    ON message_mappings (media_group_id)
    WHERE media_group_id IS NOT NULL;

-- Уникальность: одно исходное сообщение не должно ретранслироваться дважды
CREATE UNIQUE INDEX uq_message_mappings_source
    ON message_mappings (source_chat_id, source_message_id);

COMMENT ON TABLE message_mappings IS
    'Маппинг сообщений между парными группами для зеркалирования reply/edit/delete/reactions';
COMMENT ON COLUMN message_mappings.pair_key IS
    'Ключ пары из config.yaml (например, "merch_1"). Для отладки и аудита.';
COMMENT ON COLUMN message_mappings.media_group_id IS
    'NULL для одиночных сообщений; общий grouped_id для всех сообщений одного альбома.';
COMMENT ON COLUMN message_mappings.direction IS
    'merchant_to_support — событие пришло из чата мерчанта; support_to_merchant — из чата саппорта';
COMMENT ON COLUMN message_mappings.account_phone IS
    'Телефон активного аккаунта на момент создания записи. Маппинг общий между аккаунтами, поле для аудита.';

-- +goose Down
DROP TABLE IF EXISTS message_mappings;
