import { useId } from 'react';
import type { InputHTMLAttributes } from 'react';
import './input.css';

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  label: string;
  error?: string;
  hint?: string;
  required?: boolean;
}

/**
 * 表单输入原语（skill Input 模式）：label 经 useId 绑定；
 * hint/error 经 aria-describedby 关联；error 以 role="alert" 即时播报。
 */
export function Input({
  label,
  error,
  hint,
  required = false,
  type = 'text',
  id,
  ...props
}: InputProps) {
  const generatedId = useId();
  const inputId = id ?? generatedId;
  const hintId = `${inputId}-hint`;
  const errorId = `${inputId}-error`;

  return (
    <div className="input-wrapper">
      <label htmlFor={inputId} className="input-label">
        {label}
        {required && <span aria-label="required">*</span>}
      </label>

      {hint && (
        <p id={hintId} className="input-hint">
          {hint}
        </p>
      )}

      <input
        id={inputId}
        type={type}
        className={`input${error ? ' input--error' : ''}`}
        aria-required={required}
        aria-invalid={!!error}
        aria-describedby={error ? errorId : hint ? hintId : undefined}
        {...props}
      />

      {error && (
        <p id={errorId} className="input-error" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
