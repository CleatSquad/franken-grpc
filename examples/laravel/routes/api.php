<?php

use App\Http\Controllers\GrpcController;
use Illuminate\Support\Facades\Route;

Route::post('/{packageService}/{method}', [GrpcController::class, 'handle'])
    ->where('packageService', '.+\..+');
